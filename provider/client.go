package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	maxResponseBytes = 8 << 20
	// Stay below the host's two-minute watch-sync RPC deadline so a slow
	// Scrob request still comes back as a provider fault.
	defaultRequestTimeout = 110 * time.Second
	// Scrob's backend listens on localhost only; its web frontend proxies
	// API calls under this prefix and passes X-Api-Key through.
	proxyPrefix = "/api/proxy"
)

// apiClient serves one RPC; it is not shared across calls.
type apiClient struct {
	// root is the Scrob URL the operator entered, without the proxy prefix.
	root *url.URL
	api  *url.URL
	key  string
	http *http.Client
}

// apiError carries the HTTP status and the fault it maps to by default, so a
// caller can give one status (404, 409) a meaning of its own.
type apiError struct {
	status int
	fault  *pluginv1.WatchSyncFault
	// detail is Scrob's decoded error body, kept only for statuses a caller
	// inspects. It never reaches a fault message.
	detail json.RawMessage
}

func newAPIClient(rawBaseURL, key string, httpClient *http.Client) (*apiClient, error) {
	root, err := normalizeBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	api := *root
	api.Path = strings.TrimRight(root.Path, "/") + proxyPrefix
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}
	// A request without a valid session is redirected to Scrob's login page;
	// following it would turn an auth problem into an unreadable HTML body.
	noRedirects := *httpClient
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &apiClient{root: root, api: &api, key: strings.TrimSpace(key), http: &noRedirects}, nil
}

// normalizeBaseURL accepts the Scrob web URL with or without the proxy
// prefix and returns it without the prefix or a trailing slash.
func normalizeBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("Scrob URL must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Scrob URL must not include credentials, a query, or a fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.Path = strings.TrimRight(strings.TrimSuffix(parsed.Path, proxyPrefix), "/")
	parsed.RawPath = ""
	return parsed, nil
}

func (c *apiClient) endpoint(path string, query url.Values) string {
	endpoint := *c.api
	endpoint.Path = strings.TrimRight(c.api.Path, "/") + "/" + strings.TrimLeft(path, "/")
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

// do sends one request and decodes a JSON response into output when it is
// non-nil. Any status outside 2xx is returned as an *apiError.
func (c *apiClient) do(ctx context.Context, method, path string, query url.Values, payload, output any) *apiError {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return &apiError{fault: permanentFault("Scrob request could not be encoded")}
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, query), body)
	if err != nil {
		return &apiError{fault: permanentFault("Scrob request could not be created")}
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Api-Key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return &apiError{fault: temporaryFault("Scrob is temporarily unreachable", 0)}
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		failure := &apiError{status: resp.StatusCode, fault: faultForHTTPResponse(resp)}
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict {
			var envelope struct {
				Detail json.RawMessage `json:"detail"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&envelope) == nil {
				failure.detail = envelope.Detail
			}
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return failure
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(output); err != nil {
		return &apiError{status: resp.StatusCode, fault: temporaryFault("Scrob returned an unreadable response", 0)}
	}
	return nil
}

func faultForHTTPResponse(resp *http.Response) *pluginv1.WatchSyncFault {
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			SafeMessage: "Scrob rejected the API key",
		}
	case resp.StatusCode == http.StatusForbidden:
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED,
			SafeMessage: "Scrob denied access to this request",
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
			SafeMessage: "Scrob rate limit reached",
			RetryAfter:  durationpb.New(retryAfter(resp.Header.Get("Retry-After"))),
		}
	case resp.StatusCode == http.StatusRequestTimeout:
		return temporaryFault("Scrob request timed out", 0)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Scrob's frontend redirects requests it does not let through to its
		// login page, which means the URL does not reach the API proxy.
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
			SafeMessage: "Scrob redirected the request; check that the URL points to the Scrob web app",
		}
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusUnprocessableEntity:
		return invalidRequestFault(fmt.Sprintf("Scrob rejected the request (HTTP %d)", resp.StatusCode))
	case resp.StatusCode >= http.StatusInternalServerError:
		return temporaryFault("Scrob is temporarily unavailable", 0)
	default:
		return permanentFault(fmt.Sprintf("Scrob request failed (HTTP %d)", resp.StatusCode))
	}
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(at))
	}
	return 0
}

func temporaryFault(message string, retryAfter time.Duration) *pluginv1.WatchSyncFault {
	fault := &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
		SafeMessage: message,
	}
	if retryAfter > 0 {
		fault.RetryAfter = durationpb.New(retryAfter)
	}
	return fault
}

func permanentFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
		SafeMessage: message,
	}
}

func invalidRequestFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
		SafeMessage: message,
	}
}
