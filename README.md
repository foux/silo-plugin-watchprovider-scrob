# Scrob watch-provider plugin for Silo

Connects Silo profiles to a self-hosted [Scrob](https://github.com/ellite/scrob) instance through Silo's `watch_sync_provider.v1` plugin contract.

## Capabilities

- Exports completed movie and episode watches to Scrob, with their original watch time.
- Removes a watch from Scrob when it is marked unwatched in Silo.
- Imports Scrob's watch history, including plays Scrob received from other media servers.
- Imports resume progress from Scrob's Continue Watching list.
- Syncs movie, series and episode ratings in both directions.
- Shows live Silo playback in Scrob's Now Playing bar. Scrob forwards it to the services connected to it, such as Trakt or Simkl.

The plugin uses only Scrob's existing HTTP API and needs no change to Scrob. It does not sync favorites or watchlists.

## Setup

1. Install the plugin in Silo.
2. In Scrob, open the Connections page and copy the key from its API Key section. Scrob creates the key with each account.
3. In Silo's watch-provider settings, connect each profile with a Scrob URL and that Scrob user's API key.

The Scrob URL is the address of the Scrob web app as the Silo server reaches it, for example `http://scrob:7330` when both run on the same Docker network. The plugin sends its API calls through the web app's `/api/proxy` path, so a URL that already ends in `/api/proxy` also works.

The plugin needs Scrob 2.17.0 or later.

Scrob pushes every watch it records to the services connected to it. If Scrob already syncs with Trakt or Simkl, do not also connect those services to Silo directly. Silo would send them the watches it imports from Scrob a second time.

## How the sync works

### Watch history

Silo recognizes a watch it imports as one it already has only when both timestamps are identical. The plugin therefore makes Scrob store Silo's own watch time:

- A completed watch is written to Scrob's history at Silo's watch time, in whole UTC seconds, which is the precision of Silo's history.
- Scrob answers a repeated write for the same title within the user's duplicate window with 409. The plugin reports that as "no change", so a retried event is not recorded twice.
- On import, Silo skips any watch that is already in its history, including every watch that it exported to Scrob.

Silo's export does not only send Silo's own playback. It sends every watch in the profile's history that did not come from this plugin, including plays Silo imported from a media server. Those plays usually carry a different time than the one Scrob has for the same viewing. Scrob would record each one as a new play and push it to every service connected to it. The plugin therefore skips an exported watch in two cases:

- The watch is older than the export cutoff. The plugin sets the cutoff when a profile connects, so Silo's history from before the connection stays out of Scrob.
- Scrob already has a watch of the title at the same time or later.

Skipped watches are reported to Silo as unchanged and are not retried.

If Silo also imports history directly from a media server that sends its plays to Scrob, such as through Silo's Jellyfin or Plex webhook sync, those plays reach Silo twice: once directly and once from Scrob. Scrob already collects them, so let Silo get them from Scrob and remove the direct sync.

### Live playback

Start, pause and resume go to Scrob's player webhook (`/webhooks/kodi`, which Scrob's Kodi add-on also uses). Scrob resolves the title and forwards the event to Trakt and the other services connected to it.

When playback finishes, the plugin records the watch at Silo's time and then dismisses the Now Playing session. It does not send a stop to the player webhook, for two reasons:

- The webhook would stamp the watch with the time Scrob received the stop. Silo would then import that watch as a second play.
- Trakt would receive a scrobbled play in addition to the one Scrob pushes from its history.

For playback that stops before the end, the plugin reports the stop position, and Scrob keeps it as resume progress. If the position is already past Scrob's 90% watched mark, the plugin only dismisses the session, because Silo has not counted the playback as a watch.

### Episodes

The plugin first identifies an episode by its own TMDB and TVDB IDs. If Scrob does not know the episode yet, the plugin falls back to the series IDs with season and episode numbers. Scrob reads those numbers in its own episode order, which is TMDB's order unless the show comes from TheTVDB. For a show whose TVDB and TMDB orders differ, this fallback only gives the right episode when Silo's library uses the TMDB order.

### Imports

| State | Source in Scrob | Mode |
|---|---|---|
| Watched | `GET /history`, newest first | Incremental, with a full pass at most once a day |
| Progress | `GET /history/continue-watching` | Latest state of every in-progress item |
| Ratings | `GET /ratings` | Complete snapshot |

The history import is incremental because Scrob's history has no "changed since" filter. Each import reads back to 48 hours before the newest watch the previous import saw. A full pass at most once a day then catches watches that another service added to Scrob with an older date.

The ratings import covers movies, series and episodes. Season ratings have no Silo equivalent and are left out. Scrob scores from 0 to 10 and may use decimals; the plugin rounds half up and clamps to Silo's whole 1–10 scale. A title that is missing from a complete snapshot counts as unrated.

## Limitations

- Scrob records a movie only by its TMDB ID. A movie that has only an IMDb ID in Silo cannot be exported.
- Scrob rates an episode only once it tracks that episode. Rating an episode Scrob has never seen is rejected.
- Scrob does not tell an API-key client its username. The connection is named after the Scrob profile's display name, or the server address when the display name is empty.

## Development

```sh
make build   # ./plugin for the host platform
make test    # go test ./...
make lint    # golangci-lint run ./...
```

The plugin builds against `silo-plugin-sdk` v0.17.0, the version the Silo host pins.

To release, set the new version in `manifest.json`, commit, and push a matching `vX.Y.Z` tag. The release workflow checks that the tag matches the manifest, runs the tests, builds the three binaries, and publishes them with their SHA-256 checksums as a GitHub release.

## License

[AGPL-3.0-only](LICENSE)
