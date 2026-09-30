package provider

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	opStart     = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START
	opPause     = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE
	opStop      = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP
	opWatched   = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED
	opUnwatched = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED
)

func playback(operation pluginv1.WatchSyncOperation, media *pluginv1.WatchSyncMedia, position, duration float64) *pluginv1.WatchSyncEvent {
	e := event("scrobble:"+operation.String(), operation, media)
	e.PlaybackSessionId = "abc123"
	e.PositionSeconds = position
	e.DurationSeconds = duration
	return e
}

func nowPlaying(keys ...string) http.HandlerFunc {
	sessions := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		sessions = append(sessions, map[string]any{"session_key": key, "source": "kodi"})
	}
	return ok(map[string]any{"now_playing": sessions})
}

func TestMarkWatchedRecordsAMovieAtSiloTimeInWholeUTCSeconds(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", ok(map[string]any{"status": "ok"}))

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, movieMedia())))

	if result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
	body := fake.calls("POST /history")[0].body
	want := map[string]any{"media_type": "movie", "tmdb_id": 603.0, "watched_at": "2026-09-01T20:15:05Z", "completed": true}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %v, want %v", body, want)
	}
}

func TestMarkWatchedTreatsScrobDuplicateAsAlreadyApplied(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", duplicateWatch())

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, movieMedia())))

	if result.GetStatus() != statusNoChange {
		t.Fatalf("result = %v", result)
	}
}

func TestMarkWatchedRejectsAnotherConflict(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", respond(http.StatusConflict, map[string]any{"detail": "something else"}))

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, movieMedia())))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("result = %v", result)
	}
}

func TestMarkWatchedRejectsAMovieWithoutTMDBID(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	media := movieMedia()
	media.ExternalIds = map[string]string{"imdb": "tt0133093"}

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, media)))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED || len(fake.routesCalled()) != 0 {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
}

func TestMarkWatchedFindsAnEpisodeByItsOwnIDsFirst(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", ok(map[string]any{"status": "ok"}))

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, episodeMedia())))

	if result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
	calls := fake.calls("POST /history")
	if len(calls) != 1 {
		t.Fatalf("POST /history calls = %d, want 1", len(calls))
	}
	want := map[string]any{
		"media_type": "episode", "tmdb_id": 63057.0, "tvdb_id": 3436461.0,
		"watched_at": "2026-09-01T20:15:05Z", "completed": true,
	}
	if !reflect.DeepEqual(calls[0].body, want) {
		t.Fatalf("body = %v, want %v", calls[0].body, want)
	}
}

func TestMarkWatchedFallsBackToSeriesAndNumbersForAnEpisodeScrobDoesNotKnow(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	attempts := 0
	fake.handle("POST /history", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			notFound("Episode context required (series_tmdb_id, season_number, episode_number)")(w, r)
			return
		}
		ok(map[string]any{"status": "ok"})(w, r)
	})

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, episodeMedia())))

	if result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
	calls := fake.calls("POST /history")
	want := map[string]any{
		"media_type": "episode", "series_tmdb_id": 1399.0, "series_tvdb_id": 121361.0,
		"season_number": 1.0, "episode_number": 2.0,
		"watched_at": "2026-09-01T20:15:05Z", "completed": true,
	}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1].body, want) {
		t.Fatalf("calls = %v, want the second to be %v", calls, want)
	}
}

func TestMarkWatchedRejectsAnEpisodeScrobCannotFindEitherWay(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", notFound("Episode not found on TMDB"))

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, episodeMedia())))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED || len(fake.calls("POST /history")) != 2 {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
}

func TestStartSendsSeriesIDsWithTheEpisodeTMDBID(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /webhooks/kodi", ok(map[string]any{"status": "ok", "event": "play"}))

	result := onlyResult(t, apply(t, fake, playback(opStart, episodeMedia(), 0, 3000)))

	if result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
	body := fake.calls("POST /webhooks/kodi")[0].body
	if body["event"] != "playback_started" || body["session_id"] != "silo-abc123" || body["total_seconds"] != 3000.0 {
		t.Fatalf("body = %v", body)
	}
	item := body["item"].(map[string]any)
	wantIDs := map[string]any{"tmdb": "63057", "tvdb": "121361", "imdb": "tt0944947"}
	if item["type"] != "episode" || item["season"] != 1.0 || item["episode"] != 2.0 || !reflect.DeepEqual(item["uniqueid"], wantIDs) {
		t.Fatalf("item = %v", item)
	}
}

func TestStartFromASavedPositionResumes(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /webhooks/kodi", ok(map[string]any{"status": "ok"}))

	onlyResult(t, apply(t, fake, playback(opStart, movieMedia(), 1200.4, 6000)))

	body := fake.calls("POST /webhooks/kodi")[0].body
	if body["event"] != "playback_resumed" || body["position_seconds"] != 1200.0 {
		t.Fatalf("body = %v", body)
	}
}

func TestPauseIsForwarded(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /webhooks/kodi", ok(map[string]any{"status": "ok"}))

	onlyResult(t, apply(t, fake, playback(opPause, movieMedia(), 1200, 6000)))

	if body := fake.calls("POST /webhooks/kodi")[0].body; body["event"] != "playback_paused" {
		t.Fatalf("body = %v", body)
	}
}

func TestLiveEventScrobCannotIdentifyIsRejected(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /webhooks/kodi", ok(map[string]any{"status": "ignored", "reason": "could not identify media"}))

	result := onlyResult(t, apply(t, fake, playback(opStart, movieMedia(), 0, 6000)))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("result = %v", result)
	}
}

func TestCompletedStopRecordsAtSiloTimeAndDismissesOnlyItsSession(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", ok(map[string]any{"status": "ok"}))
	fake.handle("GET /history/now-playing", nowPlaying("kodi:7:silo-abc123", "kodi:7:silo-other", "jellyfin:7:xyz"))
	fake.handle("DELETE /history/session/kodi:7:silo-abc123", ok(map[string]any{"status": "ok"}))
	stop := playback(opStop, episodeMedia(), 2950, 3000)
	stop.Completed = true
	stop.WatchHistoryId = "history-1"

	result := onlyResult(t, apply(t, fake, stop))

	if result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
	want := []string{"POST /history", "GET /history/now-playing", "DELETE /history/session/kodi:7:silo-abc123"}
	if got := fake.routesCalled(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if at := fake.calls("POST /history")[0].body["watched_at"]; at != "2026-09-01T20:15:05Z" {
		t.Fatalf("watched_at = %v", at)
	}
}

func TestCompletedStopKeepsScrobDuplicateAsNoChange(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", duplicateWatch())
	fake.handle("GET /history/now-playing", nowPlaying())
	stop := playback(opStop, movieMedia(), 5990, 6000)
	stop.Completed = true
	stop.WatchHistoryId = "history-1"

	if result := onlyResult(t, apply(t, fake, stop)); result.GetStatus() != statusNoChange {
		t.Fatalf("result = %v", result)
	}
}

func TestCompletedStopWithoutHistoryIDStillRecordsTheWatch(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", ok(map[string]any{"status": "ok"}))
	fake.handle("GET /history/now-playing", nowPlaying("kodi:7:silo-abc123"))
	fake.handle("DELETE /history/session/kodi:7:silo-abc123", ok(map[string]any{"status": "ok"}))
	stop := playback(opStop, movieMedia(), 5990, 6000)
	stop.Completed = true

	result := onlyResult(t, apply(t, fake, stop))

	if result.GetStatus() != statusApplied || len(fake.calls("POST /history")) != 1 {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
}

func TestCompletedStopThatScrobRejectsStillEndsTheSession(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", notFound("Movie not found locally and no tmdb_id given to create it"))
	fake.handle("GET /history/now-playing", nowPlaying("kodi:7:silo-abc123"))
	fake.handle("DELETE /history/session/kodi:7:silo-abc123", ok(map[string]any{"status": "ok"}))
	stop := playback(opStop, movieMedia(), 5990, 6000)
	stop.Completed = true
	stop.WatchHistoryId = "history-1"

	result := onlyResult(t, apply(t, fake, stop))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("result = %v", result)
	}
	if len(fake.calls("DELETE /history/session/kodi:7:silo-abc123")) != 1 {
		t.Fatalf("requests = %v", fake.routesCalled())
	}
}

func TestCompletedStopThatScrobCannotTakeYetIsRetriedWithTheSessionOpen(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", respond(http.StatusServiceUnavailable, nil))
	stop := playback(opStop, movieMedia(), 5990, 6000)
	stop.Completed = true
	stop.WatchHistoryId = "history-1"

	result := onlyResult(t, apply(t, fake, stop))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY || len(fake.calls("GET /history/now-playing")) != 0 {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
}

func TestIncompleteStopReportsThePosition(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /webhooks/kodi", ok(map[string]any{"status": "ok"}))

	onlyResult(t, apply(t, fake, playback(opStop, movieMedia(), 1800, 6000)))

	body := fake.calls("POST /webhooks/kodi")[0].body
	if body["event"] != "playback_stopped" || body["position_seconds"] != 1800.0 || body["total_seconds"] != 6000.0 {
		t.Fatalf("body = %v", body)
	}
}

func TestIncompleteStopPastScrobThresholdOnlyEndsTheSession(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /history/now-playing", nowPlaying("kodi:7:silo-abc123"))
	fake.handle("DELETE /history/session/kodi:7:silo-abc123", ok(map[string]any{"status": "ok"}))

	onlyResult(t, apply(t, fake, playback(opStop, movieMedia(), 5500, 6000)))

	if len(fake.calls("POST /webhooks/kodi")) != 0 || len(fake.calls("POST /history")) != 0 {
		t.Fatalf("requests = %v", fake.routesCalled())
	}
}

func TestMarkUnwatchedDeletesOnlyTheWatchAtThatTime(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /history/item-events", itemEventsAt("2026-09-03T10:00:00", "2026-09-01T20:15:05", nil))
	fake.handle("DELETE /history/event/11", ok(map[string]any{"status": "ok"}))

	result := onlyResult(t, apply(t, fake, event("unwatched:1", opUnwatched, episodeMedia())))

	if result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
	if query := fake.calls("GET /history/item-events")[0].query; query != "media_type=episode&tmdb_id=63057" {
		t.Fatalf("query = %q", query)
	}
	if len(fake.calls("DELETE /history/event/11")) != 1 {
		t.Fatalf("requests = %v", fake.routesCalled())
	}
}

func TestMarkUnwatchedWithoutAMatchingWatchIsNoChange(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)

	if result := onlyResult(t, apply(t, fake, event("unwatched:1", opUnwatched, movieMedia()))); result.GetStatus() != statusNoChange {
		t.Fatalf("result = %v", result)
	}
}

func TestMarkWatchedSkipsAWatchScrobAlreadyHasALaterOrEqualOneFor(t *testing.T) {
	t.Parallel()
	for name, existing := range map[string]string{
		"later watch": "2026-09-02T08:00:00",
		"same second": "2026-09-01T20:15:05.900000",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeScrob(t)
			fake.handle("GET /history/item-events", itemEventsAt(existing))

			result := onlyResult(t, apply(t, fake, event("e1", opWatched, movieMedia())))

			if result.GetStatus() != statusNoChange || len(fake.calls("POST /history")) != 0 {
				t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
			}
			if query := fake.calls("GET /history/item-events")[0].query; query != "media_type=movie&tmdb_id=603" {
				t.Fatalf("query = %q", query)
			}
		})
	}
}

func TestMarkWatchedRecordsAWatchNewerThanEverythingScrobHas(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /history/item-events", itemEventsAt("2026-08-30T20:00:00", nil))
	fake.handle("POST /history", ok(map[string]any{"status": "ok"}))

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, episodeMedia())))

	if result.GetStatus() != statusApplied || len(fake.calls("POST /history")) != 1 {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
}

func TestMarkWatchedForAnEpisodeWithoutItsOwnIDsGoesStraightToTheSeries(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /history", ok(map[string]any{"status": "ok"}))
	media := episodeMedia()
	media.ExternalIds = nil

	result := onlyResult(t, apply(t, fake, event("e1", opWatched, media)))

	want := []string{"POST /history"}
	if result.GetStatus() != statusApplied || !reflect.DeepEqual(fake.routesCalled(), want) {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
}

func TestMarkWatchedSkipsHistoryFromBeforeTheExportCutoff(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	auth := fake.authContext()
	auth.Credentials.SecretAttributes[exportSinceAttribute] = "2026-09-29T10:00:00Z"

	response, err := NewServer(fake.server.Client()).ApplyEvents(t.Context(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: auth,
		Events:  []*pluginv1.WatchSyncEvent{event("e1", opWatched, movieMedia())},
	})
	if err != nil {
		t.Fatal(err)
	}

	if result := onlyResult(t, response); result.GetStatus() != statusNoChange || len(fake.routesCalled()) != 0 {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
	if response.GetUpdatedCredentials() != nil {
		t.Fatal("a connection that has a cutoff must keep it")
	}
}

func TestAConnectionWithoutACutoffStartsItNow(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	auth := fake.authContext()
	delete(auth.Credentials.SecretAttributes, exportSinceAttribute)
	now := time.Date(2026, time.September, 29, 12, 0, 0, 500, time.UTC)
	server := &Server{http: fake.server.Client(), now: func() time.Time { return now }}

	response, err := server.ApplyEvents(t.Context(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: auth,
		Events:  []*pluginv1.WatchSyncEvent{event("e1", opWatched, movieMedia())},
	})
	if err != nil {
		t.Fatal(err)
	}

	if result := onlyResult(t, response); result.GetStatus() != statusNoChange || len(fake.routesCalled()) != 0 {
		t.Fatalf("result = %v, requests = %v", result, fake.routesCalled())
	}
	updated := response.GetUpdatedCredentials()
	if updated.GetSecretAttributes()[exportSinceAttribute] != "2026-09-29T12:00:00Z" ||
		updated.GetAccessToken() != testKey || updated.GetSecretAttributes()[configBaseURL] != fake.server.URL {
		t.Fatalf("updated credentials = %v", updated)
	}
}
