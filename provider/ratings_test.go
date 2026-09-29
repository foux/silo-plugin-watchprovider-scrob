package provider

import (
	"net/http"
	"reflect"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	opSetRating    = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING
	opRemoveRating = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING
)

func seriesMedia() *pluginv1.WatchSyncMedia {
	return &pluginv1.WatchSyncMedia{
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES,
		ExternalIds: map[string]string{"tmdb": "1399", "tvdb": "121361"},
	}
}

func rating(id int, mediaType string, tmdb any, score float64, season any) map[string]any {
	return map[string]any{
		"id": id, "media": map[string]any{"id": 100 + id, "tmdb_id": tmdb, "type": mediaType, "title": "t"},
		"season_number": season, "episode_order": nil, "rating": score, "rated_at": "2026-09-01T20:20:00.5",
	}
}

func TestSetRatingSendsTheSeriesRating(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /ratings", ok(map[string]any{"id": 1}))
	set := event("rating:1", opSetRating, seriesMedia())
	set.Rating = 8

	if result := onlyResult(t, apply(t, fake, set)); result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
	want := map[string]any{"media_type": "series", "tmdb_id": 1399.0, "rating": 8.0}
	if body := fake.calls("POST /ratings")[0].body; !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %v, want %v", body, want)
	}
}

func TestSetRatingOnAnEpisodeScrobDoesNotTrackIsRejected(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("POST /ratings", respond(http.StatusBadRequest, map[string]any{"detail": "Cannot create media row for episodes via rating"}))
	set := event("rating:1", opSetRating, episodeMedia())
	set.Rating = 7

	result := onlyResult(t, apply(t, fake, set))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("result = %v", result)
	}
	body := fake.calls("POST /ratings")[0].body
	if body["media_type"] != "episode" || body["tmdb_id"] != 63057.0 || body["tvdb_id"] != 3436461.0 {
		t.Fatalf("body = %v", body)
	}
}

func TestSetRatingRejectsAnOutOfRangeValue(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	set := event("rating:1", opSetRating, movieMedia())

	result := onlyResult(t, apply(t, fake, set))

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED || len(fake.routesCalled()) != 0 {
		t.Fatalf("result = %v", result)
	}
}

func TestRemovingAnAbsentRatingIsNoChange(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("DELETE /ratings", notFound("Rating not found"))

	result := onlyResult(t, apply(t, fake, event("rating:2", opRemoveRating, movieMedia())))

	if result.GetStatus() != statusNoChange {
		t.Fatalf("result = %v", result)
	}
	if query := fake.calls("DELETE /ratings")[0].query; query != "media_type=movie&tmdb_id=603" {
		t.Fatalf("query = %q", query)
	}
}

func TestRemoveRatingFallsBackToTheReportedKey(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("DELETE /ratings", ok(map[string]any{"status": "deleted"}))
	remove := event("rating:3", opRemoveRating, &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE})
	remove.ProviderItemKey = "movie:tmdb:603"

	if result := onlyResult(t, apply(t, fake, remove)); result.GetStatus() != statusApplied {
		t.Fatalf("result = %v", result)
	}
}

func TestRatingImportIsACompleteSnapshotOfWholeTitles(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	fake.handle("GET /ratings", ok(map[string]any{"results": []map[string]any{
		rating(4, "series", 1399, 9.5, nil),
		rating(3, "series", 1399, 6, 2), // a season rating
		rating(2, "movie", 603, 7.4, nil),
		rating(1, "episode", 63057, 0, nil),
		rating(5, "movie", nil, 8, nil), // no TMDB ID
	}}))

	items, _, complete := traverse(t, NewServer(fake.server.Client()), fake, kindRating, "", 2)

	if !complete {
		t.Fatal("ratings must be a complete snapshot")
	}
	got := map[string]int32{}
	for _, item := range items {
		got[item.GetProviderItemKey()] = item.GetRating().GetRating()
	}
	want := map[string]int32{"episode:tmdb:63057": 1, "movie:tmdb:603": 7, "series:tmdb:1399": 10}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ratings = %v, want %v", got, want)
	}
	if items[0].GetRating().GetRatedAt() == nil {
		t.Fatal("rated_at is missing")
	}
}

func TestRatingImportAbandonsASnapshotThatShiftedBetweenPages(t *testing.T) {
	t.Parallel()
	fake := newFakeScrob(t)
	calls := 0
	fake.handle("GET /ratings", func(w http.ResponseWriter, r *http.Request) {
		calls++
		rows := []map[string]any{rating(1, "movie", 1, 5, nil), rating(2, "movie", 2, 5, nil), rating(3, "movie", 3, 5, nil)}
		if calls > 1 {
			rows = rows[1:] // rating 1 was removed after the first page
		}
		ok(map[string]any{"results": rows})(w, r)
	})
	server := NewServer(fake.server.Client())
	first, err := server.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context: fake.authContext(), PageSize: 1, StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindRating},
	})
	if err != nil {
		t.Fatal(err)
	}

	second, err := server.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context: fake.authContext(), PageSize: 1, PageToken: first.GetNextPageToken(),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindRating},
	})
	if err != nil {
		t.Fatal(err)
	}

	if second.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Fatalf("second page = %v", second)
	}
}

func TestSiloRatingRoundsHalfUpAndClamps(t *testing.T) {
	t.Parallel()
	for score, want := range map[float64]int32{0: 1, 0.4: 1, 4.5: 5, 7.49: 7, 9.5: 10, 10: 10} {
		if got := siloRating(score); got != want {
			t.Errorf("siloRating(%v) = %d, want %d", score, got, want)
		}
	}
}
