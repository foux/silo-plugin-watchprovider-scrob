package provider

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type scrobRating struct {
	ID    int64 `json:"id"`
	Media struct {
		TMDBID *int64 `json:"tmdb_id"`
		Type   string `json:"type"`
		Title  string `json:"title"`
	} `json:"media"`
	SeasonNumber *int32  `json:"season_number"`
	EpisodeOrder *string `json:"episode_order"`
	Rating       float64 `json:"rating"`
	RatedAt      string  `json:"rated_at"`
}

type ratingRequest struct {
	MediaType string  `json:"media_type"`
	TMDBID    *int64  `json:"tmdb_id,omitempty"`
	TVDBID    *int64  `json:"tvdb_id,omitempty"`
	Rating    float64 `json:"rating"`
}

// ratingTarget names an item the way Scrob's ratings API addresses it: the
// Scrob media type plus a TMDB ID, or a TVDB ID for an episode.
type ratingTarget struct {
	mediaType string
	tmdb      int64
	tvdb      int64
}

func (t ratingTarget) key() string {
	return t.mediaType + ":tmdb:" + strconv.FormatInt(t.tmdb, 10)
}

// applyRating sets or clears a movie, series or episode rating. Both are
// convergent writes: Scrob's POST is an upsert, and clearing a rating that is
// already gone is a no-op.
func applyRating(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (pluginv1.WatchSyncApplyStatus, *pluginv1.WatchSyncFault) {
	target, fault := ratingTargetFor(event)
	if fault != nil {
		return 0, fault
	}
	if event.GetOperation() == pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING {
		query := url.Values{}
		query.Set("media_type", target.mediaType)
		if target.tmdb != 0 {
			query.Set("tmdb_id", strconv.FormatInt(target.tmdb, 10))
		} else {
			query.Set("tvdb_id", strconv.FormatInt(target.tvdb, 10))
		}
		failure := client.do(ctx, http.MethodDelete, "/ratings", query, nil, nil)
		switch {
		case failure == nil:
			return statusApplied, nil
		case failure.status == http.StatusNotFound:
			return statusNoChange, nil
		default:
			return 0, failure.fault
		}
	}
	rating := event.GetRating()
	if rating < 1 || rating > 10 {
		return 0, invalidRequestFault("Ratings must be whole numbers from 1 to 10")
	}
	failure := client.do(ctx, http.MethodPost, "/ratings", nil, ratingRequest{
		MediaType: target.mediaType,
		TMDBID:    int64Pointer(target.tmdb),
		TVDBID:    int64Pointer(target.tvdb),
		Rating:    float64(rating),
	}, nil)
	switch {
	case failure == nil:
		return statusApplied, nil
	case failure.status == http.StatusNotFound, failure.status == http.StatusBadRequest:
		// Scrob creates a missing movie or series from TMDB, but it rates an
		// episode only once it already tracks that episode.
		return 0, invalidRequestFault("Scrob could not find this title to rate")
	default:
		return 0, failure.fault
	}
}

func ratingTargetFor(event *pluginv1.WatchSyncEvent) (ratingTarget, *pluginv1.WatchSyncFault) {
	media := event.GetMedia()
	own := parseIDs(media.GetExternalIds())
	target := ratingTarget{tmdb: own.tmdb}
	switch media.GetMediaType() {
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
		target.mediaType = "movie"
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES:
		target.mediaType = "series"
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
		target.mediaType = "episode"
		target.tvdb = own.tvdb
	default:
		return ratingTarget{}, invalidRequestFault("Scrob rates movies, series and episodes only")
	}
	if target.tmdb == 0 {
		// A removal can address the record this plugin reported earlier.
		if kind, id, ok := parseRatingKey(event.GetProviderItemKey()); ok && kind == target.mediaType {
			target.tmdb = id
		}
	}
	if target.tmdb == 0 && target.tvdb == 0 {
		return ratingTarget{}, invalidRequestFault("Scrob needs a TMDB ID to find this rating")
	}
	return target, nil
}

func parseRatingKey(key string) (string, int64, bool) {
	parts := strings.Split(strings.TrimSpace(key), ":")
	if len(parts) != 3 || parts[1] != "tmdb" {
		return "", 0, false
	}
	id := positiveInt(parts[2])
	return parts[0], id, id != 0
}

// listRatings reports every movie, series and episode rating as one complete
// snapshot, so the host treats a title missing from it as unrated. Season
// ratings have no Silo equivalent and are left out. Each page reads the whole
// list again; if a rating added or removed in between has shifted it, the
// snapshot is abandoned instead of silently dropping a title.
func listRatings(ctx context.Context, client *apiClient, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	type ratingsToken struct {
		Offset int   `json:"offset"`
		LastID int64 `json:"last_id"`
	}
	var token ratingsToken
	if strings.TrimSpace(req.GetPageToken()) != "" {
		if !decodeToken(req.GetPageToken(), &token) || token.Offset <= 0 {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: invalidRequestFault("Scrob page token is invalid")}, nil
		}
	}
	var list struct {
		Results []scrobRating `json:"results"`
	}
	if failure := client.do(ctx, http.MethodGet, "/ratings", nil, nil, &list); failure != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: failure.fault}, nil
	}
	ratings := make([]scrobRating, 0, len(list.Results))
	for _, rating := range list.Results {
		if rating.SeasonNumber != nil || rating.EpisodeOrder != nil || value(rating.Media.TMDBID) == 0 {
			continue
		}
		switch rating.Media.Type {
		case "movie", "series", "episode":
			ratings = append(ratings, rating)
		}
	}
	sort.Slice(ratings, func(i, j int) bool { return ratings[i].ID < ratings[j].ID })
	if token.Offset > 0 && (token.Offset > len(ratings) || ratings[token.Offset-1].ID != token.LastID) {
		return &pluginv1.WatchSyncListRemoteStateResponse{
			Fault: temporaryFault("Scrob ratings changed during the import; it will start over", 0),
		}, nil
	}

	response := &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}
	end := min(len(ratings), token.Offset+pageSize(req.GetPageSize()))
	for _, rating := range ratings[token.Offset:end] {
		target := ratingTarget{mediaType: rating.Media.Type, tmdb: value(rating.Media.TMDBID)}
		state := &pluginv1.WatchSyncRemoteRatingState{Rating: siloRating(rating.Rating)}
		if at, ok := parseScrobTimestamp(rating.RatedAt); ok {
			state.RatedAt = timestamppb.New(at)
		}
		response.Items = append(response.Items, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: target.key(),
			Media: &pluginv1.WatchSyncMedia{
				MediaType:   siloMediaType(rating.Media.Type),
				Title:       rating.Media.Title,
				ExternalIds: ids{tmdb: target.tmdb}.externalIDs(),
			},
			Rating: state,
		})
	}
	if end < len(ratings) {
		response.NextPageToken = encodeToken(ratingsToken{Offset: end, LastID: ratings[end-1].ID})
	}
	return response, nil
}

func siloMediaType(scrobType string) pluginv1.WatchSyncMediaType {
	switch scrobType {
	case "movie":
		return pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE
	case "series":
		return pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
	default:
		return pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE
	}
}

// siloRating converts Scrob's 0-10 score, which may carry decimals, to
// Silo's whole 1-10 scale: half up, then clamped.
func siloRating(score float64) int32 {
	return int32(min(10, max(1, math.Floor(score+0.5))))
}
