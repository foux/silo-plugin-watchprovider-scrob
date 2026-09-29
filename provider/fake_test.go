package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testKey = "test-api-key"

// recorded is one request the fake Scrob received, with the proxy prefix
// already stripped from the path.
type recorded struct {
	method string
	path   string
	query  string
	body   map[string]any
}

// fakeScrob serves Scrob's API under /api/proxy. Routes are keyed by
// "METHOD /path"; an unknown route fails the test.
type fakeScrob struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []recorded
	routes   map[string]http.HandlerFunc
}

func newFakeScrob(t *testing.T) *fakeScrob {
	t.Helper()
	fake := &fakeScrob{t: t, routes: map[string]http.HandlerFunc{}}
	// Every exported watch first looks up the title's watches. By default
	// Scrob has none; a test overrides the route to give it some.
	fake.handle("GET /history/item-events", itemEventsAt())
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeScrob) handle(route string, handler http.HandlerFunc) { f.routes[route] = handler }

func (f *fakeScrob) serve(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, proxyPrefix)
	if !ok {
		f.t.Errorf("request outside the API proxy: %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Header.Get("X-Api-Key") != testKey {
		http.Error(w, `{"detail":"Could not validate credentials"}`, http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("request body is not JSON: %s", raw)
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, recorded{method: r.Method, path: path, query: r.URL.RawQuery, body: body})
	f.mu.Unlock()
	handler, ok := f.routes[r.Method+" "+path]
	if !ok {
		f.t.Errorf("unexpected request: %s %s", r.Method, path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	handler(w, r)
}

func (f *fakeScrob) calls(route string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, request := range f.requests {
		if request.method+" "+request.path == route {
			out = append(out, request)
		}
	}
	return out
}

func (f *fakeScrob) routesCalled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests))
	for _, request := range f.requests {
		out = append(out, request.method+" "+request.path)
	}
	return out
}

func (f *fakeScrob) authContext() *pluginv1.WatchSyncAuthenticatedContext {
	return &pluginv1.WatchSyncAuthenticatedContext{
		CapabilityId: capabilityID,
		// The cutoff predates every test watch, so tests see only the other guard.
		Credentials: credentials(testKey, f.server.URL, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)),
	}
}

func respond(status int, payload any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func ok(payload any) http.HandlerFunc { return respond(http.StatusOK, payload) }

func notFound(detail string) http.HandlerFunc {
	return respond(http.StatusNotFound, map[string]any{"detail": detail})
}

// itemEventsAt answers GET /history/item-events with one watch per time.
func itemEventsAt(times ...any) http.HandlerFunc {
	events := make([]map[string]any, 0, len(times))
	for index, at := range times {
		events = append(events, map[string]any{"id": 10 + index, "watched_at": at})
	}
	return ok(map[string]any{"watched": len(events) > 0, "events": events})
}

func duplicateWatch() http.HandlerFunc {
	return respond(http.StatusConflict, map[string]any{"detail": map[string]any{
		"error": "duplicate_watch", "existing_event_id": 1, "existing_watched_at": "2026-09-01T20:15:05",
	}})
}

func movieMedia() *pluginv1.WatchSyncMedia {
	return &pluginv1.WatchSyncMedia{
		MediaItemId: "movie-tmdb-603",
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		ExternalIds: map[string]string{"tmdb": "603", "imdb": "tt0133093", "tvdb": "169"},
	}
}

func episodeMedia() *pluginv1.WatchSyncMedia {
	return &pluginv1.WatchSyncMedia{
		MediaItemId:       "episode-tvdb-121361-1-2",
		MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		ExternalIds:       map[string]string{"tmdb": "63057", "tvdb": "3436461", "imdb": "tt1668746"},
		SeriesExternalIds: map[string]string{"tmdb": "1399", "tvdb": "121361", "imdb": "tt0944947"},
		SeasonNumber:      1,
		EpisodeNumber:     2,
	}
}

// watchTime has a sub-second part, which Scrob must never see.
var watchTime = time.Date(2026, time.September, 1, 20, 15, 5, 420_000_000, time.UTC)

func event(id string, operation pluginv1.WatchSyncOperation, media *pluginv1.WatchSyncMedia) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:    id,
		Operation:  operation,
		OccurredAt: timestamppb.New(watchTime),
		Media:      media,
	}
}

func apply(t *testing.T, fake *fakeScrob, events ...*pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyEventsResponse {
	t.Helper()
	response, err := NewServer(fake.server.Client()).ApplyEvents(t.Context(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: fake.authContext(),
		Events:  events,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func onlyResult(t *testing.T, response *pluginv1.WatchSyncApplyEventsResponse) *pluginv1.WatchSyncApplyResult {
	t.Helper()
	if response.GetFault() != nil {
		t.Fatalf("batch fault = %v", response.GetFault())
	}
	if len(response.GetResults()) != 1 {
		t.Fatalf("results = %v, want one", response.GetResults())
	}
	return response.GetResults()[0]
}
