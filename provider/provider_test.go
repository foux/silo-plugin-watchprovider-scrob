package provider

import (
	"net/http"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func exchange(t *testing.T, fake *fakeScrob, baseURL, key string) *pluginv1.WatchSyncCredentialResponse {
	t.Helper()
	response, err := NewServer(fake.server.Client()).ExchangeAPIKey(t.Context(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{Values: map[string]string{configBaseURL: baseURL}},
		ApiKey:         key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestExchangeAPIKeyValidatesThroughTheProxyAndKeepsTheURL(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /profile/me", ok(map[string]any{"display_name": "Foux", "avatar_url": "/profile/avatar/7"}))

	response := exchange(t, fake, fake.server.URL+"/", testKey)

	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	creds := response.GetCredentials()
	if creds.GetAccessToken() != testKey || creds.GetSecretAttributes()[configBaseURL] != fake.server.URL ||
		creds.GetSecretAttributes()[exportSinceAttribute] == "" {
		t.Fatalf("credentials = %v", creds)
	}
	account := response.GetAccount()
	if account.GetUsername() != "Foux" || !strings.HasPrefix(account.GetExternalSubject(), "scrob:") {
		t.Fatalf("account = %v", account)
	}
	if strings.Contains(account.GetExternalSubject(), testKey) {
		t.Fatal("the account subject exposes the API key")
	}
}

func TestExchangeAPIKeyAcceptsAURLThatAlreadyEndsWithTheProxyPrefix(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /profile/me", ok(map[string]any{"display_name": nil}))

	response := exchange(t, fake, fake.server.URL+proxyPrefix, testKey)

	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	if got := response.GetCredentials().GetSecretAttributes()[configBaseURL]; got != fake.server.URL {
		t.Fatalf("stored URL = %q, want %q", got, fake.server.URL)
	}
	if response.GetAccount().GetUsername() == "" {
		t.Fatal("an account without a display name needs a fallback name")
	}
}

func TestExchangeAPIKeyReportsARejectedKey(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)

	response := exchange(t, fake, fake.server.URL, "wrong-key")

	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault = %v", response.GetFault())
	}
	if strings.Contains(response.GetFault().GetSafeMessage(), "wrong-key") {
		t.Fatal("the fault message exposes the API key")
	}
}

func TestExchangeAPIKeyDoesNotFollowARedirectToTheLoginPage(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /profile/me", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})

	response := exchange(t, fake, fake.server.URL, testKey)

	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v", response.GetFault())
	}
	if calls := fake.routesCalled(); len(calls) != 1 {
		t.Fatalf("requests = %v, want only the profile request", calls)
	}
}

func TestExchangeAPIKeyRejectsAPageThatIsNotScrob(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /profile/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not scrob</html>"))
	})

	response := exchange(t, fake, fake.server.URL, testKey)

	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v", response.GetFault())
	}
}

func TestExchangeAPIKeyRejectsAnInvalidURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "scrob:7330", "ftp://scrob", "http://user:pass@scrob:7330", "http://scrob:7330/?a=b"} {
		response, err := NewServer(nil).ExchangeAPIKey(t.Context(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
			CapabilityId:   capabilityID,
			ProviderConfig: &pluginv1.WatchSyncProviderConfig{Values: map[string]string{configBaseURL: raw}},
			ApiKey:         testKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
			t.Errorf("URL %q: fault = %v", raw, response.GetFault())
		}
	}
}

func TestApplyEventsTurnsARejectedKeyIntoAConnectionFault(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	auth := fake.authContext()
	auth.Credentials.AccessToken = "revoked"

	response, err := NewServer(fake.server.Client()).ApplyEvents(t.Context(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: auth,
		Events:  []*pluginv1.WatchSyncEvent{event("e1", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED, movieMedia())},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL || len(response.GetResults()) != 0 {
		t.Fatalf("response = %v", response)
	}
}

func TestApplyEventsRetriesAServerError(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", respond(http.StatusInternalServerError, map[string]any{"detail": "boom"}))

	result := onlyResult(t, apply(t, fake, event("e1", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED, movieMedia())))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatalf("result = %v", result)
	}
}
