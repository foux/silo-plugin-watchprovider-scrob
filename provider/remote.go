package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100 // Scrob's history page size limit
	// Scrob has no "changed since" filter, and an import from another service
	// can add watches dated in the past. An incremental import reads the
	// newest watches back to this far before the last one it saw; a full
	// import, run at most once per fullScanInterval, catches older additions.
	incrementalOverlap = 48 * time.Hour
	fullScanInterval   = 24 * time.Hour
	// Scrob returns at most this many in-progress items in one call.
	progressLimit = 1000
)

// scrobEvent is one row of Scrob's history or continue-watching lists.
type scrobEvent struct {
	ID              int64      `json:"id"`
	WatchedAt       *string    `json:"watched_at"`
	PlayCount       int32      `json:"play_count"`
	ProgressPercent *float64   `json:"progress_percent"`
	Media           scrobMedia `json:"media"`
}

type scrobMedia struct {
	ID            int64   `json:"id"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	TMDBID        *int64  `json:"tmdb_id"`
	TVDBID        *int64  `json:"tvdb_id"`
	IMDbID        *string `json:"imdb_id"`
	ReleaseDate   *string `json:"release_date"`
	SeasonNumber  *int32  `json:"season_number"`
	EpisodeNumber *int32  `json:"episode_number"`
	ShowTitle     string  `json:"show_title"`
	ShowTMDBID    *int64  `json:"show_tmdb_id"`
	ShowTVDBID    *int64  `json:"show_tvdb_id"`
}

// watchedCursor is the durable checkpoint of the watched import.
type watchedCursor struct {
	// HighWater is the newest watch time seen by any finished import.
	HighWater time.Time `json:"hw"`
	// FullAt is when the last full import started.
	FullAt time.Time `json:"full"`
}

// watchedToken carries one watched traversal from page to page.
type watchedToken struct {
	Full      bool      `json:"full"`
	Page      int       `json:"page"`
	HighWater time.Time `json:"hw"`
	StopAt    time.Time `json:"stop,omitempty"`
	StartedAt time.Time `json:"started"`
	PrevFull  time.Time `json:"prev_full"`
}

// listWatched pages through Scrob's history, newest first, and reports every
// completed watch as its own item. The host keeps the latest watch per title
// across the whole traversal and skips a watch whose time it already has.
func (s *Server) listWatched(ctx context.Context, client *apiClient, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	token, fault := s.watchedTraversal(req)
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	size := pageSize(req.GetPageSize())
	query := url.Values{}
	query.Set("page", strconv.Itoa(token.Page))
	query.Set("page_size", strconv.Itoa(size))
	var page struct {
		Results    []scrobEvent `json:"results"`
		TotalPages int          `json:"total_pages"`
	}
	if failure := client.do(ctx, http.MethodGet, "/history", query, nil, &page); failure != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: failure.fault}, nil
	}

	response := &pluginv1.WatchSyncListRemoteStateResponse{}
	finished := len(page.Results) < size || token.Page >= page.TotalPages
	for _, event := range page.Results {
		at, ok := timeOf(event.WatchedAt)
		if !ok {
			// Watches with an unknown date sort last and the host cannot
			// import them, so an incremental traversal is over.
			if !token.Full {
				finished = true
				break
			}
			continue
		}
		if !token.Full && at.Before(token.StopAt) {
			finished = true
			break
		}
		if at.After(token.HighWater) {
			token.HighWater = at
		}
		media, ok := siloMedia(event.Media)
		if !ok {
			continue
		}
		response.Items = append(response.Items, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: "watch:" + strconv.FormatInt(event.ID, 10),
			Media:           media,
			Watched: &pluginv1.WatchSyncRemoteWatchedState{
				PlayCount:     max(1, event.PlayCount),
				LastWatchedAt: timestamppb.New(at),
			},
		})
	}
	if finished {
		next := watchedCursor{HighWater: token.HighWater, FullAt: token.PrevFull}
		if token.Full {
			next.FullAt = token.StartedAt
		}
		response.NextCursor = encodeToken(next)
		return response, nil
	}
	token.Page++
	response.NextPageToken = encodeToken(token)
	return response, nil
}

func (s *Server) watchedTraversal(req *pluginv1.WatchSyncListRemoteStateRequest) (watchedToken, *pluginv1.WatchSyncFault) {
	if strings.TrimSpace(req.GetPageToken()) != "" {
		var token watchedToken
		if !decodeToken(req.GetPageToken(), &token) || token.Page < 2 || token.StartedAt.IsZero() {
			return watchedToken{}, invalidRequestFault("Scrob page token is invalid")
		}
		return token, nil
	}
	// A cursor this plugin cannot read restarts with a full import.
	var cursor watchedCursor
	_ = decodeToken(req.GetCursor(), &cursor)
	now := s.clock().UTC()
	token := watchedToken{Page: 1, HighWater: cursor.HighWater, StartedAt: now, PrevFull: cursor.FullAt}
	token.Full = cursor.FullAt.IsZero() || cursor.HighWater.IsZero() || now.Sub(cursor.FullAt) >= fullScanInterval
	if !token.Full {
		token.StopAt = cursor.HighWater.Add(-incrementalOverlap)
	}
	return token, nil
}

// listProgress reports Scrob's resume points. Scrob keeps one per title and
// few at a time, so every page reads the whole list again.
func listProgress(ctx context.Context, client *apiClient, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	offset := 0
	if strings.TrimSpace(req.GetPageToken()) != "" {
		var token struct {
			Offset int `json:"offset"`
		}
		if !decodeToken(req.GetPageToken(), &token) || token.Offset <= 0 {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: invalidRequestFault("Scrob page token is invalid")}, nil
		}
		offset = token.Offset
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(progressLimit))
	query.Set("include_hidden", "false")
	var list struct {
		Items []scrobEvent `json:"continue_watching"`
	}
	if failure := client.do(ctx, http.MethodGet, "/history/continue-watching", query, nil, &list); failure != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: failure.fault}, nil
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{}
	end := min(len(list.Items), offset+pageSize(req.GetPageSize()))
	for _, item := range list.Items[min(offset, end):end] {
		at, ok := timeOf(item.WatchedAt)
		if !ok || item.ProgressPercent == nil || *item.ProgressPercent <= 0 || *item.ProgressPercent >= 1 {
			continue
		}
		media, ok := siloMedia(item.Media)
		if !ok {
			continue
		}
		response.Items = append(response.Items, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: "progress:" + strconv.FormatInt(item.Media.ID, 10),
			Media:           media,
			Progress: &pluginv1.WatchSyncRemoteProgressState{
				ProgressPercent: *item.ProgressPercent * 100,
				PausedAt:        timestamppb.New(at),
			},
		})
	}
	if end < len(list.Items) {
		response.NextPageToken = encodeToken(struct {
			Offset int `json:"offset"`
		}{end})
	}
	return response, nil
}

// siloMedia converts a Scrob movie or episode. Scrob's season and episode
// numbers are its canonical order (TMDB, or TVDB for a TVDB-only episode);
// Silo matches an episode by its own IDs before it uses the numbers.
func siloMedia(media scrobMedia) (*pluginv1.WatchSyncMedia, bool) {
	own := ids{tmdb: value(media.TMDBID), tvdb: value(media.TVDBID)}
	if media.IMDbID != nil && strings.HasPrefix(*media.IMDbID, "tt") {
		own.imdb = *media.IMDbID
	}
	switch media.Type {
	case "movie":
		if own.empty() {
			return nil, false
		}
		return &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			Title:       media.Title,
			Year:        yearOf(media.ReleaseDate),
			ExternalIds: own.externalIDs(),
		}, true
	case "episode":
		series := ids{tmdb: value(media.ShowTMDBID), tvdb: value(media.ShowTVDBID)}
		numbered := media.SeasonNumber != nil && media.EpisodeNumber != nil
		if own.empty() && (series.empty() || !numbered) {
			return nil, false
		}
		out := &pluginv1.WatchSyncMedia{
			MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:             media.Title,
			ExternalIds:       own.externalIDs(),
			SeriesTitle:       media.ShowTitle,
			SeriesExternalIds: series.externalIDs(),
		}
		if numbered {
			out.SeasonNumber = *media.SeasonNumber
			out.EpisodeNumber = *media.EpisodeNumber
		}
		return out, true
	default:
		return nil, false
	}
}

func value(pointer *int64) int64 {
	if pointer == nil || *pointer < 0 {
		return 0
	}
	return *pointer
}

func yearOf(date *string) int32 {
	if date == nil || len(*date) < 4 {
		return 0
	}
	year, err := strconv.Atoi((*date)[:4])
	if err != nil {
		return 0
	}
	return int32(year)
}

func timeOf(value *string) (time.Time, bool) {
	if value == nil {
		return time.Time{}, false
	}
	return parseScrobTimestamp(*value)
}

func pageSize(requested int32) int {
	if requested <= 0 {
		return defaultPageSize
	}
	return min(maxPageSize, int(requested))
}

func encodeToken(token any) string {
	encoded, _ := json.Marshal(token)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeToken(value string, into any) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) == 0 {
		return false
	}
	return json.Unmarshal(decoded, into) == nil
}
