package provider

import (
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// ids holds one item's external IDs in the namespaces Scrob understands.
// TMDB and TVDB IDs are numeric in Scrob; a value that does not parse is
// treated as absent.
type ids struct {
	tmdb int64
	tvdb int64
	imdb string
}

func (i ids) empty() bool { return i.tmdb == 0 && i.tvdb == 0 && i.imdb == "" }

func parseIDs(input map[string]string) ids {
	var out ids
	for key, value := range input {
		key = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(key)), "_id")
		value = strings.TrimSpace(value)
		switch key {
		case "tmdb":
			out.tmdb = positiveInt(value)
		case "tvdb":
			out.tvdb = positiveInt(value)
		case "imdb":
			if strings.HasPrefix(value, "tt") {
				out.imdb = value
			}
		}
	}
	return out
}

func positiveInt(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// externalIDs renders IDs back into Silo's namespace keys, leaving empty ones out.
func (i ids) externalIDs() map[string]string {
	out := map[string]string{}
	if i.tmdb > 0 {
		out["tmdb"] = strconv.FormatInt(i.tmdb, 10)
	}
	if i.tvdb > 0 {
		out["tvdb"] = strconv.FormatInt(i.tvdb, 10)
	}
	if i.imdb != "" {
		out["imdb"] = i.imdb
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scrobTimestamp formats a watch time the way Scrob stores it. Scrob drops a
// timestamp's offset without converting it, so the value must be UTC. It is
// truncated to the second because Silo keeps its own history at second
// precision, and Silo only recognises an imported watch as one it already
// has when both timestamps are identical.
func scrobTimestamp(at time.Time) string {
	return at.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

// parseScrobTimestamp reads Scrob's naive ISO timestamps, which are UTC.
func parseScrobTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func isEpisode(media *pluginv1.WatchSyncMedia) bool {
	return media.GetMediaType() == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE
}

func isMovie(media *pluginv1.WatchSyncMedia) bool {
	return media.GetMediaType() == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE
}

func hasEpisodeNumbers(media *pluginv1.WatchSyncMedia) bool {
	return media.GetSeasonNumber() >= 0 && media.GetEpisodeNumber() >= 1
}

func int64Pointer(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}

func int32Pointer(value int32) *int32 { return &value }
