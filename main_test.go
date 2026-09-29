package main

import (
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

func TestManifestDeclaresPerConnectionScrobServer(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	capabilities := parsed.GetCapabilities()
	if len(capabilities) != 1 || capabilities[0].GetId() != "scrob" {
		t.Fatalf("capabilities = %v", capabilities)
	}
	schemas := capabilities[0].GetConfigSchema()
	if len(schemas) != 1 || schemas[0].GetKey() != "scrob" || !schemas[0].GetRequired() {
		t.Fatalf("connection config schemas = %#v", schemas)
	}
	if len(parsed.GetGlobalConfigSchema()) != 0 {
		t.Fatalf("global config schemas = %#v, want none", parsed.GetGlobalConfigSchema())
	}
}

func TestManifestAdvertisesOnlyWhatThePluginImplements(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	descriptor := parsed.GetCapabilities()[0].GetWatchSyncProvider()
	if !descriptor.GetExportWatched() || !descriptor.GetExportUnwatched() || !descriptor.GetImportWatched() ||
		!descriptor.GetImportProgress() || !descriptor.GetImportRatings() || !descriptor.GetExportRatings() ||
		!descriptor.GetScrobblePlayback() {
		t.Fatalf("descriptor = %v", descriptor)
	}
	if descriptor.GetImportFavorites() || descriptor.GetExportFavorites() || descriptor.GetImportWatchlist() || descriptor.GetExportWatchlist() {
		t.Fatalf("descriptor advertises favorites or watchlist: %v", descriptor)
	}
	if auth := descriptor.GetAuthMethods(); len(auth) != 1 || auth[0] != pluginv1.WatchSyncAuthMethod_WATCH_SYNC_AUTH_METHOD_API_KEY {
		t.Fatalf("auth methods = %v", auth)
	}
	if descriptor.GetMaxBatchSize() < 1 || descriptor.GetMaxBatchSize() > 100 {
		t.Fatalf("max batch size = %d, want 1..100 (Scrob's page limit)", descriptor.GetMaxBatchSize())
	}
}
