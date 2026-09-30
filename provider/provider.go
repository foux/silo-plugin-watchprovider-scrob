// Package provider implements Silo's watch_sync_provider.v1 contract on top of
// Scrob's HTTP API.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	capabilityID  = "scrob"
	configBaseURL = "scrob.base_url"
	// exportSinceAttribute is the connection's export cutoff, kept with the
	// host-owned credentials: see exportWatch.
	exportSinceAttribute = "scrob.export_since"
	// ApplyEvents stops starting new events after this long, or sooner when
	// the call's deadline, less a margin, comes first. Every Scrob history
	// write fans out to the services the user connected to Scrob (Trakt,
	// Simkl, media servers), so one write can take several seconds.
	syncBudget = 90 * time.Second
	// The margin keeps the finished results ahead of the host's deadline. It
	// shrinks with a short deadline: a stop from Silo's Jellyfin-compatible
	// API arrives with only five seconds, and a fixed five-second margin
	// would leave no time at all and turn every such stop into a retry that
	// fails the same way.
	syncDeadlineMargin         = 5 * time.Second
	syncDeadlineMarginFraction = 5
)

type Server struct {
	pluginv1.UnimplementedWatchSyncProviderServer
	http *http.Client
	now  func() time.Time
}

func NewServer(httpClient *http.Client) *Server {
	return &Server{http: httpClient}
}

func (s *Server) ExchangeAPIKey(ctx context.Context, req *pluginv1.WatchSyncExchangeAPIKeyRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	client, fault := s.client(req.GetCapabilityId(), req.GetProviderConfig(), req.GetApiKey(), "")
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	account, fault := fetchAccount(ctx, client)
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: credentials(client.key, client.root.String(), s.clock()),
		Account:     account,
	}, nil
}

// RefreshCredentials only revalidates: a Scrob API key does not expire.
func (s *Server) RefreshCredentials(ctx context.Context, req *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	account, fault := fetchAccount(ctx, client)
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: cloneCredentials(req.GetContext().GetCredentials()),
		Account:     account,
	}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *pluginv1.WatchSyncGetAccountRequest) (*pluginv1.WatchSyncGetAccountResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: fault}, nil
	}
	account, fault := fetchAccount(ctx, client)
	return &pluginv1.WatchSyncGetAccountResponse{Account: account, Fault: fault}, nil
}

func (s *Server) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncApplyEventsResponse{Fault: fault}, nil
	}
	// Bound the whole call, including a request still in flight when the
	// budget runs out, below the host's RPC deadline: an interrupted request
	// becomes a retry for its event, and the finished results still reach the
	// host in time.
	budget := syncTimeBox(ctx)
	ctx, cancel := context.WithTimeout(ctx, syncRequestLimit(ctx))
	defer cancel()
	startedAt := s.clock()
	events := req.GetEvents()
	response := &pluginv1.WatchSyncApplyEventsResponse{
		Results: make([]*pluginv1.WatchSyncApplyResult, 0, len(events)),
	}
	// A connection made before the cutoff existed starts it now, and the host
	// keeps it from then on.
	exportSince, known := exportCutoff(req.GetContext().GetCredentials())
	if !known {
		exportSince = s.clock().UTC().Truncate(time.Second)
		updated := cloneCredentials(req.GetContext().GetCredentials())
		if updated.SecretAttributes == nil {
			updated.SecretAttributes = map[string]string{}
		}
		updated.SecretAttributes[exportSinceAttribute] = exportSince.Format(time.RFC3339)
		response.UpdatedCredentials = updated
	}
	for index, event := range events {
		// The first event always runs: with a tight deadline the budget can
		// be zero, and returning nothing but retries would never make progress.
		if index > 0 && s.clock().Sub(startedAt) >= budget {
			for _, deferred := range events[index:] {
				response.Results = append(response.Results, resultFromFault(deferred.GetEventId(),
					temporaryFault("Scrob sync time limit reached; the event will be retried", 0)))
			}
			break
		}
		result, connectionFault := s.applyEvent(ctx, client, event, exportSince)
		if connectionFault != nil {
			return &pluginv1.WatchSyncApplyEventsResponse{UpdatedCredentials: response.UpdatedCredentials, Fault: connectionFault}, nil
		}
		response.Results = append(response.Results, result)
	}
	return response, nil
}

func (s *Server) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	kind, fault := requestedStateKind(req.GetStateKinds())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, syncRequestLimit(ctx))
	defer cancel()
	switch kind {
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED:
		return s.listWatched(ctx, client, req)
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS:
		return listProgress(ctx, client, req)
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING:
		return listRatings(ctx, client, req)
	default:
		return &pluginv1.WatchSyncListRemoteStateResponse{
			Fault: invalidRequestFault("Scrob does not support the requested state family"),
		}, nil
	}
}

func (s *Server) applyEvent(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent, exportSince time.Time) (*pluginv1.WatchSyncApplyResult, *pluginv1.WatchSyncFault) {
	if event == nil || strings.TrimSpace(event.GetEventId()) == "" {
		return rejectedResult("", "Watch event ID is required"), nil
	}
	var fault *pluginv1.WatchSyncFault
	status := pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
	switch event.GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
		pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE:
		status, fault = scrobbleLive(ctx, client, event)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
		status, fault = scrobbleStop(ctx, client, event)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED:
		status, fault = exportWatch(ctx, client, event, exportSince)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED:
		status, fault = removeWatch(ctx, client, event)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING,
		pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING:
		status, fault = applyRating(ctx, client, event)
	default:
		fault = invalidRequestFault("Scrob does not support this watch operation")
	}
	if fault != nil {
		if connectionWide(fault) {
			return nil, fault
		}
		return resultFromFault(event.GetEventId(), fault), nil
	}
	return &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId(), Status: status}, nil
}

// fetchAccount validates the key. Scrob does not expose the key owner's
// username to an API-key caller, so the account is named after the profile's
// display name, or the server when that is empty.
func fetchAccount(ctx context.Context, client *apiClient) (*pluginv1.WatchSyncAccount, *pluginv1.WatchSyncFault) {
	var profile struct {
		DisplayName *string `json:"display_name"`
		AvatarURL   *string `json:"avatar_url"`
	}
	if failure := client.do(ctx, http.MethodGet, "/profile/me", nil, nil, &profile); failure != nil {
		if failure.status == http.StatusOK {
			return nil, invalidRequestFault("The URL does not answer like a Scrob server")
		}
		return nil, failure.fault
	}
	name := client.root.Host
	if profile.DisplayName != nil && strings.TrimSpace(*profile.DisplayName) != "" {
		name = strings.TrimSpace(*profile.DisplayName)
	}
	return &pluginv1.WatchSyncAccount{
		ExternalSubject: accountSubject(client),
		Username:        name,
		DisplayName:     name,
		ProfileUrl:      client.root.String(),
	}, nil
}

// accountSubject is a stable identifier for the Scrob account behind a key.
// It is a truncated hash, so the key itself never leaves the plugin.
func accountSubject(client *apiClient) string {
	sum := sha256.Sum256([]byte(client.root.String() + "\x00" + client.key))
	return "scrob:" + hex.EncodeToString(sum[:12])
}

func (s *Server) authenticatedClient(auth *pluginv1.WatchSyncAuthenticatedContext) (*apiClient, *pluginv1.WatchSyncFault) {
	if auth == nil || auth.GetCredentials() == nil {
		return nil, invalidRequestFault("Scrob credentials are required")
	}
	baseURL := auth.GetCredentials().GetSecretAttributes()[configBaseURL]
	return s.client(auth.GetCapabilityId(), auth.GetProviderConfig(), auth.GetCredentials().GetAccessToken(), baseURL)
}

func (s *Server) client(requestedCapability string, config *pluginv1.WatchSyncProviderConfig, key, connectionBaseURL string) (*apiClient, *pluginv1.WatchSyncFault) {
	if requestedCapability != capabilityID {
		return nil, invalidRequestFault("Unknown Scrob capability")
	}
	if strings.TrimSpace(key) == "" {
		return nil, invalidRequestFault("Scrob API key is required")
	}
	baseURL := strings.TrimSpace(connectionBaseURL)
	if baseURL == "" && config != nil {
		baseURL = config.GetValues()[configBaseURL]
		if baseURL == "" {
			baseURL = config.GetSecretValues()[configBaseURL]
		}
	}
	client, err := newAPIClient(baseURL, key, s.http)
	if err != nil {
		return nil, invalidRequestFault(err.Error())
	}
	return client, nil
}

func credentials(key, baseURL string, exportSince time.Time) *pluginv1.WatchSyncCredentials {
	return &pluginv1.WatchSyncCredentials{
		AccessToken: strings.TrimSpace(key),
		TokenType:   "ApiKey",
		SecretAttributes: map[string]string{
			configBaseURL:        baseURL,
			exportSinceAttribute: exportSince.UTC().Truncate(time.Second).Format(time.RFC3339),
		},
	}
}

func exportCutoff(creds *pluginv1.WatchSyncCredentials) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, creds.GetSecretAttributes()[exportSinceAttribute])
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func cloneCredentials(value *pluginv1.WatchSyncCredentials) *pluginv1.WatchSyncCredentials {
	if value == nil {
		return nil
	}
	return &pluginv1.WatchSyncCredentials{
		AccessToken: value.GetAccessToken(), RefreshToken: value.GetRefreshToken(),
		ExpiresAt: value.GetExpiresAt(), TokenType: value.GetTokenType(),
		Scopes:           append([]string(nil), value.GetScopes()...),
		SecretAttributes: cloneMap(value.GetSecretAttributes()),
	}
}

func requestedStateKind(kinds []pluginv1.WatchSyncRemoteStateKind) (pluginv1.WatchSyncRemoteStateKind, *pluginv1.WatchSyncFault) {
	if len(kinds) == 0 {
		return pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED, nil
	}
	if len(kinds) != 1 {
		return 0, invalidRequestFault("Scrob accepts one state family per traversal")
	}
	return kinds[0], nil
}

// syncTimeBox returns how long a call keeps starting new work: syncBudget,
// cut short so the results still reach the host before the call's deadline.
func syncTimeBox(ctx context.Context) time.Duration {
	budget := syncBudget
	if remaining, ok := timeBeforeDeadline(ctx); ok {
		budget = min(budget, remaining)
	}
	return budget
}

// syncRequestLimit bounds every request of one call: the client's request
// timeout, cut to end a margin before the call's deadline.
func syncRequestLimit(ctx context.Context) time.Duration {
	limit := defaultRequestTimeout
	if remaining, ok := timeBeforeDeadline(ctx); ok {
		limit = min(limit, remaining)
	}
	return limit
}

// timeBeforeDeadline is the time left before the call's deadline, less the
// margin: syncDeadlineMargin, or a fraction of the remaining time when the
// deadline is closer than that.
func timeBeforeDeadline(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	remaining := max(time.Until(deadline), 0)
	margin := min(syncDeadlineMargin, remaining/syncDeadlineMarginFraction)
	return remaining - margin, true
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func connectionWide(fault *pluginv1.WatchSyncFault) bool {
	return fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL ||
		fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED
}

func resultFromFault(eventID string, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncApplyResult {
	status := pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
	if fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY ||
		fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED {
		status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
	}
	return &pluginv1.WatchSyncApplyResult{EventId: eventID, Status: status, Fault: fault}
}

func rejectedResult(eventID, message string) *pluginv1.WatchSyncApplyResult {
	return resultFromFault(eventID, invalidRequestFault(message))
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
