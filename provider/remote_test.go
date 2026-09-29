package provider

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	kindWatched  = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED
	kindProgress = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS
	kindRating   = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING
)

func historyEvent(id int, watchedAt any, media map[string]any) map[string]any {
	return map[string]any{"id": id, "watched_at": watchedAt, "completed": true, "play_count": 1, "media": media}
}

func movieRow() map[string]any {
	return map[string]any{"id": 55, "type": "movie", "title": "The Matrix", "tmdb_id": 603, "imdb_id": "tt0133093", "tvdb_id": nil, "release_date": "1999-03-30"}
}

func episodeRow() map[string]any {
	return map[string]any{
		"id": 56, "type": "episode", "title": "The Kingsroad", "tmdb_id": 63057, "tvdb_id": 3436461, "imdb_id": "tt1668746",
		"season_number": 1, "episode_number": 2, "show_title": "Game of Thrones", "show_tmdb_id": 1399, "show_tvdb_id": 121361,
	}
}

// pagedHistory serves the given rows newest first, as Scrob does.
func pagedHistory(rows []map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		start := min(len(rows), (page-1)*size)
		end := min(len(rows), start+size)
		totalPages := (len(rows) + size - 1) / size
		ok(map[string]any{"page": page, "page_size": size, "total_results": len(rows), "total_pages": totalPages, "results": rows[start:end]})(w, r)
	}
}

// traverse runs one whole traversal the way the host does and returns every
// item and the final cursor.
func traverse(t *testing.T, server *Server, fake *fakeScrob, kind pluginv1.WatchSyncRemoteStateKind, cursor string, pageSize int32) ([]*pluginv1.WatchSyncRemoteState, string, bool) {
	t.Helper()
	var items []*pluginv1.WatchSyncRemoteState
	pageToken := ""
	for range 100 {
		response, err := server.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context: fake.authContext(), Cursor: cursor, PageToken: pageToken, PageSize: pageSize,
			StateKinds: []pluginv1.WatchSyncRemoteStateKind{kind},
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFault() != nil {
			t.Fatalf("fault = %v", response.GetFault())
		}
		items = append(items, response.GetItems()...)
		if response.GetNextPageToken() == "" {
			return items, response.GetNextCursor(), response.GetCompleteSnapshot()
		}
		if response.GetNextCursor() != "" {
			t.Fatal("a durable cursor came before the final page")
		}
		pageToken = response.GetNextPageToken()
	}
	t.Fatal("traversal did not finish")
	return nil, "", false
}

func TestFirstWatchedImportReadsTheWholeHistory(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /history", pagedHistory([]map[string]any{
		historyEvent(3, "2026-09-03T10:00:00", episodeRow()),
		historyEvent(2, "2026-09-02T09:30:00.123456", movieRow()),
		historyEvent(1, nil, movieRow()),
	}))
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	server := &Server{http: fake.server.Client(), now: func() time.Time { return now }}

	items, cursor, complete := traverse(t, server, fake, kindWatched, "", 2)

	if complete {
		t.Fatal("a watched import is never a complete snapshot")
	}
	if len(items) != 2 {
		t.Fatalf("items = %v, want the two dated watches", items)
	}
	episode := items[0]
	if episode.GetProviderItemKey() != "watch:3" ||
		episode.GetWatched().GetLastWatchedAt().AsTime() != time.Date(2026, time.September, 3, 10, 0, 0, 0, time.UTC) ||
		episode.GetWatched().GetPlayCount() != 1 {
		t.Fatalf("episode = %v", episode)
	}
	media := episode.GetMedia()
	if media.GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE ||
		media.GetExternalIds()["tmdb"] != "63057" || media.GetExternalIds()["tvdb"] != "3436461" ||
		media.GetSeriesExternalIds()["tmdb"] != "1399" || media.GetSeriesExternalIds()["tvdb"] != "121361" ||
		media.GetSeasonNumber() != 1 || media.GetEpisodeNumber() != 2 || media.GetSeriesTitle() != "Game of Thrones" {
		t.Fatalf("episode media = %v", media)
	}
	movie := items[1].GetMedia()
	if movie.GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE || movie.GetYear() != 1999 ||
		movie.GetExternalIds()["imdb"] != "tt0133093" {
		t.Fatalf("movie media = %v", movie)
	}
	var saved watchedCursor
	if !decodeToken(cursor, &saved) || !saved.FullAt.Equal(now) ||
		!saved.HighWater.Equal(time.Date(2026, time.September, 3, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("cursor = %+v", saved)
	}
}

func TestIncrementalWatchedImportStopsBeforeTheOverlapWindow(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	highWater := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	fake.handle("GET /history", pagedHistory([]map[string]any{
		historyEvent(9, "2026-09-21T08:00:00", movieRow()),
		historyEvent(8, "2026-09-19T12:00:00", movieRow()), // inside the 48 h overlap
		historyEvent(7, "2026-09-18T11:00:00", movieRow()), // before it
		historyEvent(6, "2026-09-01T11:00:00", movieRow()),
	}))
	now := highWater.Add(20 * time.Hour)
	server := &Server{http: fake.server.Client(), now: func() time.Time { return now }}
	previousFull := now.Add(-time.Hour)
	cursor := encodeToken(watchedCursor{HighWater: highWater, FullAt: previousFull})

	items, next, _ := traverse(t, server, fake, kindWatched, cursor, 50)

	if len(items) != 2 || items[0].GetProviderItemKey() != "watch:9" || items[1].GetProviderItemKey() != "watch:8" {
		t.Fatalf("items = %v", items)
	}
	var saved watchedCursor
	if !decodeToken(next, &saved) || !saved.FullAt.Equal(previousFull) ||
		!saved.HighWater.Equal(time.Date(2026, time.September, 21, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("cursor = %+v", saved)
	}
}

func TestWatchedImportRunsInFullOnceADay(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /history", pagedHistory([]map[string]any{
		historyEvent(2, "2026-09-21T08:00:00", movieRow()),
		historyEvent(1, "2025-01-01T08:00:00", movieRow()),
	}))
	now := time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)
	server := &Server{http: fake.server.Client(), now: func() time.Time { return now }}
	cursor := encodeToken(watchedCursor{HighWater: now.Add(-time.Hour), FullAt: now.Add(-25 * time.Hour)})

	items, next, _ := traverse(t, server, fake, kindWatched, cursor, 50)

	var saved watchedCursor
	if len(items) != 2 || !decodeToken(next, &saved) || !saved.FullAt.Equal(now) {
		t.Fatalf("items = %d, cursor = %+v", len(items), saved)
	}
}

func TestWatchedImportRejectsAForeignPageToken(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	response, err := NewServer(fake.server.Client()).ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context: fake.authContext(), PageToken: "not-a-token",
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindWatched},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("response = %v", response)
	}
}

func TestProgressImportConvertsFractionsToPercent(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /history/continue-watching", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != strconv.Itoa(progressLimit) {
			t.Errorf("query = %v", r.URL.Query())
		}
		ok(map[string]any{"continue_watching": []map[string]any{
			{"id": 1, "watched_at": "2026-09-28T21:00:00", "progress_percent": 0.42, "progress_seconds": 2520, "completed": false, "media": movieRow()},
			{"id": 2, "watched_at": "2026-09-28T20:00:00", "progress_percent": 0.25, "completed": false, "media": episodeRow()},
			{"id": 3, "watched_at": "2026-09-28T19:00:00", "progress_percent": 1.0, "completed": false, "media": movieRow()},
		}})(w, r)
	})

	items, _, _ := traverse(t, NewServer(fake.server.Client()), fake, kindProgress, "", 1)

	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	first := items[0].GetProgress()
	if first.GetProgressPercent() != 42 || first.GetPausedAt().AsTime() != time.Date(2026, time.September, 28, 21, 0, 0, 0, time.UTC) {
		t.Fatalf("progress = %v", first)
	}
	if items[1].GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE || items[1].GetProgress().GetProgressPercent() != 25 {
		t.Fatalf("second = %v", items[1])
	}
}

func TestRemoteStateRejectsSeveralKindsAtOnce(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	response, err := NewServer(fake.server.Client()).ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    fake.authContext(),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindWatched, kindProgress},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault() == nil || len(fake.routesCalled()) != 0 {
		t.Fatalf("response = %v", response)
	}
}

func TestHistoryRequestsCarryPageAndSize(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /history", pagedHistory(nil))

	traverse(t, NewServer(fake.server.Client()), fake, kindWatched, "", 500)

	query, _ := url.ParseQuery(fake.calls("GET /history")[0].query)
	if query.Get("page") != "1" || query.Get("page_size") != strconv.Itoa(maxPageSize) {
		t.Fatalf("query = %v", query)
	}
}
