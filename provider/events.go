package provider

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	statusApplied  = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
	statusNoChange = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE

	// Scrob's generic player webhook, shared with its Kodi add-on. It takes
	// external IDs, resolves the title server-side, drives Scrob's Now
	// Playing bar, and forwards live scrobbles to the services the user
	// connected to Scrob.
	playerWebhookPath = "/webhooks/kodi"
	// Scrob records a watch when a stop reaches this fraction of the runtime.
	scrobWatchedFraction = 0.90
)

type playerPayload struct {
	Event           string     `json:"event"`
	SessionID       string     `json:"session_id"`
	PositionSeconds int64      `json:"position_seconds"`
	TotalSeconds    int64      `json:"total_seconds"`
	Item            playerItem `json:"item"`
}

type playerItem struct {
	Type      string            `json:"type"`
	UniqueID  map[string]string `json:"uniqueid,omitempty"`
	Title     string            `json:"title,omitempty"`
	Year      int32             `json:"year,omitempty"`
	ShowTitle string            `json:"showtitle,omitempty"`
	Season    *int32            `json:"season,omitempty"`
	Episode   *int32            `json:"episode,omitempty"`
}

type playerResponse struct {
	Status string `json:"status"`
}

// scrobbleLive forwards a start or pause to Scrob's player webhook.
func scrobbleLive(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	name := "playback_paused"
	if event.GetOperation() == pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START {
		// Silo also sends a start when a paused session resumes. Scrob's
		// resume event opens the session like a start and keeps the position.
		name = "playback_started"
		if event.GetPositionSeconds() > 0 {
			name = "playback_resumed"
		}
	}
	return sendPlayerEvent(ctx, client, name, event)
}

// scrobbleStop ends a Scrob session.
//
// A completed playback is recorded with POST /history at Silo's own watch
// time instead of through the player webhook, which would stamp the watch
// with the time Scrob received the stop. Silo deduplicates an imported watch
// only against an identical timestamp, so a watch stamped by Scrob would come
// back into Silo as a second play on the next import. The session is then
// dismissed rather than stopped: a webhook stop would forward a second,
// scrobbled play to Trakt and the other services on top of the one Scrob's
// history write already pushes.
func scrobbleStop(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	if !event.GetCompleted() {
		if fraction(event) >= scrobWatchedFraction {
			// Scrob would count this stop as a watch that Silo did not count.
			return dismissSession(ctx, client, event)
		}
		return sendPlayerEvent(ctx, client, "playback_stopped", event)
	}
	status := statusApplied
	// Without a history ID the host has not queued this play for export, and
	// its reconciliation sends it later as MARK_WATCHED with the exact time
	// of its history row. Recording it here would use the stop's time instead.
	if event.GetWatchHistoryId() != "" {
		var fault *pluginv1.WatchSyncFault
		status, fault = recordWatch(ctx, client, event.GetMedia(), event.GetOccurredAt())
		if fault != nil && (connectionWide(fault) || isRetryable(fault)) {
			return 0, fault
		}
		if fault != nil {
			// The watch cannot be recorded, but the session must still end.
			if _, closeFault := dismissSession(ctx, client, event); closeFault != nil && connectionWide(closeFault) {
				return 0, closeFault
			}
			return 0, fault
		}
	}
	if _, fault := dismissSession(ctx, client, event); fault != nil {
		return 0, fault
	}
	return status, nil
}

func sendPlayerEvent(ctx context.Context, client *apiClient, name string, event *pluginv1.WatchSyncEvent) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	item, fault := playerItemFor(event.GetMedia())
	if fault != nil {
		return 0, fault
	}
	payload := playerPayload{
		Event:           name,
		SessionID:       sessionID(event),
		PositionSeconds: int64(math.Round(max(event.GetPositionSeconds(), 0))),
		TotalSeconds:    int64(math.Round(max(event.GetDurationSeconds(), 0))),
		Item:            item,
	}
	var response playerResponse
	if failure := client.do(ctx, http.MethodPost, playerWebhookPath, nil, payload, &response); failure != nil {
		return 0, failure.fault
	}
	if response.Status != "ok" {
		return 0, invalidRequestFault("Scrob could not identify this title")
	}
	return statusApplied, nil
}

// playerItemFor describes an item for the player webhook. For an episode the
// webhook reads the TVDB and IMDb IDs as series IDs and resolves the episode
// from the season and episode numbers; the TMDB ID is only a hint that it
// checks against the series, so the episode's own TMDB ID is the better one.
func playerItemFor(media *pluginv1.WatchSyncMedia) (playerItem, *pluginv1.WatchSyncFault) {
	switch {
	case isMovie(media):
		movie := parseIDs(media.GetExternalIds())
		if movie.empty() {
			return playerItem{}, invalidRequestFault("Movie events need a TMDB or IMDb ID")
		}
		return playerItem{
			Type: "movie", UniqueID: movie.externalIDs(),
			Title: media.GetTitle(), Year: media.GetYear(),
		}, nil
	case isEpisode(media):
		if !hasEpisodeNumbers(media) {
			return playerItem{}, invalidRequestFault("Episode events need season and episode numbers")
		}
		episode := parseIDs(media.GetExternalIds())
		series := parseIDs(media.GetSeriesExternalIds())
		unique := ids{tmdb: episode.tmdb, tvdb: series.tvdb, imdb: series.imdb}
		if unique.tmdb == 0 {
			unique.tmdb = series.tmdb
		}
		if unique.empty() {
			return playerItem{}, invalidRequestFault("Episode events need series or episode IDs")
		}
		return playerItem{
			Type: "episode", UniqueID: unique.externalIDs(),
			Title: media.GetTitle(), ShowTitle: media.GetSeriesTitle(),
			Season: int32Pointer(media.GetSeasonNumber()), Episode: int32Pointer(media.GetEpisodeNumber()),
		}, nil
	default:
		return playerItem{}, invalidRequestFault("Scrob supports movie and episode playback only")
	}
}

// sessionID keys the Scrob session. Scrob keeps a session's title for its
// whole life, so the key must be unique to one playback.
func sessionID(event *pluginv1.WatchSyncEvent) string {
	if id := strings.TrimSpace(event.GetPlaybackSessionId()); id != "" {
		return "silo-" + id
	}
	return "silo-" + event.GetMedia().GetMediaItemId()
}

func fraction(event *pluginv1.WatchSyncEvent) float64 {
	if event.GetDurationSeconds() <= 0 {
		return 0
	}
	return event.GetPositionSeconds() / event.GetDurationSeconds()
}

// dismissSession removes this playback's Now Playing session in Scrob
// without recording or forwarding anything. Scrob keys a webhook session as
// "kodi:<user id>:<session id>" and does not tell an API-key caller its user
// ID, so the key is read back from the Now Playing list.
func dismissSession(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	var nowPlaying struct {
		Sessions []struct {
			SessionKey string `json:"session_key"`
		} `json:"now_playing"`
	}
	if failure := client.do(ctx, http.MethodGet, "/history/now-playing", nil, nil, &nowPlaying); failure != nil {
		return 0, failure.fault
	}
	suffix := ":" + sessionID(event)
	status := statusNoChange
	for _, session := range nowPlaying.Sessions {
		if !strings.HasPrefix(session.SessionKey, "kodi:") || !strings.HasSuffix(session.SessionKey, suffix) {
			continue
		}
		failure := client.do(ctx, http.MethodDelete, "/history/session/"+url.PathEscape(session.SessionKey), nil, nil, nil)
		if failure != nil && failure.status != http.StatusNotFound {
			return 0, failure.fault
		}
		status = statusApplied
	}
	return status, nil
}

type watchRequest struct {
	MediaType     string `json:"media_type"`
	TMDBID        *int64 `json:"tmdb_id,omitempty"`
	TVDBID        *int64 `json:"tvdb_id,omitempty"`
	SeriesTMDBID  *int64 `json:"series_tmdb_id,omitempty"`
	SeriesTVDBID  *int64 `json:"series_tvdb_id,omitempty"`
	SeasonNumber  *int32 `json:"season_number,omitempty"`
	EpisodeNumber *int32 `json:"episode_number,omitempty"`
	WatchedAt     string `json:"watched_at"`
	Completed     bool   `json:"completed"`
}

// recordWatch adds one completed watch to Scrob's history.
//
// Scrob answers 409 when the item already has a watch within the user's
// duplicate window, which makes a retried event a no-op. An episode is looked
// up by its own IDs first. The series-and-numbers fallback lets Scrob create
// an episode it has never seen, but Scrob reads those numbers in its own
// (normally TMDB) episode order, so it is only tried second.
func recordWatch(ctx context.Context, client *apiClient, media *pluginv1.WatchSyncMedia, at *timestamppb.Timestamp) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	if at == nil || at.CheckValid() != nil {
		return 0, invalidRequestFault("Watch events need a watch time")
	}
	watchedAt := scrobTimestamp(at.AsTime())
	var attempts []watchRequest
	switch {
	case isMovie(media):
		movie := parseIDs(media.GetExternalIds())
		if movie.tmdb == 0 {
			return 0, invalidRequestFault("Scrob needs a TMDB ID to record a movie")
		}
		attempts = append(attempts, watchRequest{MediaType: "movie", TMDBID: int64Pointer(movie.tmdb)})
	case isEpisode(media):
		episode := parseIDs(media.GetExternalIds())
		series := parseIDs(media.GetSeriesExternalIds())
		if episode.tmdb != 0 || episode.tvdb != 0 {
			attempts = append(attempts, watchRequest{
				MediaType: "episode", TMDBID: int64Pointer(episode.tmdb), TVDBID: int64Pointer(episode.tvdb),
			})
		}
		if (series.tmdb != 0 || series.tvdb != 0) && hasEpisodeNumbers(media) {
			attempts = append(attempts, watchRequest{
				MediaType:    "episode",
				SeriesTMDBID: int64Pointer(series.tmdb), SeriesTVDBID: int64Pointer(series.tvdb),
				SeasonNumber: int32Pointer(media.GetSeasonNumber()), EpisodeNumber: int32Pointer(media.GetEpisodeNumber()),
			})
		}
		if len(attempts) == 0 {
			return 0, invalidRequestFault("Scrob needs episode IDs, or series IDs with season and episode numbers")
		}
	default:
		return 0, invalidRequestFault("Scrob records movie and episode watches only")
	}
	for _, attempt := range attempts {
		attempt.WatchedAt = watchedAt
		attempt.Completed = true
		failure := client.do(ctx, http.MethodPost, "/history", nil, attempt, nil)
		switch {
		case failure == nil:
			return statusApplied, nil
		case failure.status == http.StatusConflict && isDuplicateWatch(failure.detail):
			return statusNoChange, nil
		case failure.status == http.StatusNotFound:
			continue
		default:
			return 0, failure.fault
		}
	}
	return 0, invalidRequestFault("Scrob could not find this title")
}

func isDuplicateWatch(detail json.RawMessage) bool {
	var body struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(detail, &body) == nil && body.Error == "duplicate_watch"
}

// exportWatch records a watch that Silo's history reconciliation sends.
//
// That history is not only Silo's own playback: it also holds every play Silo
// imported from elsewhere, such as a media server's history, usually stamped
// with a different time than the one Scrob has for the same viewing. Scrob
// would take each as a new play and push it to every service connected to it.
// Two guards keep those out:
//
//   - A watch from before the connection's export cutoff, set when the
//     profile connects, is history Silo already had, and is skipped.
//   - A watch is recorded only when it is newer than every watch Scrob has
//     for the title, so a later copy of a viewing Scrob knows is skipped too
//     when Scrob's own copy is not older.
func exportWatch(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent, exportSince time.Time) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	at := event.GetOccurredAt()
	if at == nil || at.CheckValid() != nil {
		return 0, invalidRequestFault("Watch events need a watch time")
	}
	target := at.AsTime().UTC().Truncate(time.Second)
	if target.Before(exportSince) {
		return statusNoChange, nil
	}
	events, addressable, fault := itemEvents(ctx, client, event.GetMedia())
	if fault != nil {
		return 0, fault
	}
	if addressable {
		for _, recorded := range events {
			if watchedAt, ok := timeOf(recorded.WatchedAt); ok && !watchedAt.Truncate(time.Second).Before(target) {
				return statusNoChange, nil
			}
		}
	}
	return recordWatch(ctx, client, event.GetMedia(), at)
}

// removeWatch deletes the Scrob watch recorded for one Silo play, found by
// its watch time. Other plays of the same item stay.
func removeWatch(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	at := event.GetOccurredAt()
	if at == nil || at.CheckValid() != nil {
		return 0, invalidRequestFault("Unwatch events need the watch time")
	}
	events, addressable, fault := itemEvents(ctx, client, event.GetMedia())
	if fault != nil {
		return 0, fault
	}
	if !addressable {
		return 0, invalidRequestFault("Scrob needs a TMDB or TVDB ID to find this watch")
	}
	target := at.AsTime().UTC().Truncate(time.Second)
	status := statusNoChange
	for _, recorded := range events {
		watchedAt, ok := timeOf(recorded.WatchedAt)
		if !ok || !watchedAt.Truncate(time.Second).Equal(target) {
			continue
		}
		failure := client.do(ctx, http.MethodDelete, "/history/event/"+strconv.FormatInt(recorded.ID, 10), nil, nil, nil)
		if failure != nil && failure.status != http.StatusNotFound {
			return 0, failure.fault
		}
		status = statusApplied
	}
	return status, nil
}

type itemEvent struct {
	ID        int64   `json:"id"`
	WatchedAt *string `json:"watched_at"`
}

// itemEvents lists Scrob's completed watches of one movie or episode.
// addressable is false when the item has no TMDB or TVDB ID of its own, the
// only IDs Scrob can look a single item up by.
func itemEvents(ctx context.Context, client *apiClient, media *pluginv1.WatchSyncMedia) ([]itemEvent, bool, *pluginv1.WatchSyncFault) {
	query := url.Values{}
	switch {
	case isMovie(media):
		query.Set("media_type", "movie")
	case isEpisode(media):
		query.Set("media_type", "episode")
	default:
		return nil, false, invalidRequestFault("Scrob tracks movie and episode watches only")
	}
	own := parseIDs(media.GetExternalIds())
	switch {
	case own.tmdb != 0:
		query.Set("tmdb_id", strconv.FormatInt(own.tmdb, 10))
	case own.tvdb != 0:
		query.Set("tvdb_id", strconv.FormatInt(own.tvdb, 10))
	default:
		return nil, false, nil
	}
	var history struct {
		Events []itemEvent `json:"events"`
	}
	if failure := client.do(ctx, http.MethodGet, "/history/item-events", query, nil, &history); failure != nil {
		return nil, false, failure.fault
	}
	return history.Events, true, nil
}

func isRetryable(fault *pluginv1.WatchSyncFault) bool {
	return fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY ||
		fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED
}
