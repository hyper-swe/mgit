package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigService_New_CorruptFile_Error: a malformed config.json on disk is a
// hard error, not silently ignored.
func TestConfigService_New_CorruptFile_Error(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte("{not valid json"), 0o600))
	_, err := NewConfigService(path)
	assert.Error(t, err, "a corrupt config file must fail to load")
}

// TestConfigService_Get_TraverseIntoNonMap_NotFound: descending past a scalar
// (e.g. project.prefix.deeper) is reported as key-not-found, not a panic.
func TestConfigService_Get_TraverseIntoNonMap_NotFound(t *testing.T) {
	svc, err := NewConfigService(filepath.Join(t.TempDir(), "c.json"))
	require.NoError(t, err)
	_, err = svc.Get("project.prefix.deeper")
	assert.Error(t, err, "traversing into a scalar must be key-not-found")
}

// TestConfigService_Save_UnwritablePath_Error: Save surfaces a write failure
// (parent directory does not exist).
func TestConfigService_Save_UnwritablePath_Error(t *testing.T) {
	svc, err := NewConfigService(filepath.Join(t.TempDir(), "c.json"))
	require.NoError(t, err)
	svc.configPath = filepath.Join(t.TempDir(), "no", "such", "dir", "config.json")
	assert.Error(t, svc.Save(), "Save into a missing directory must error")
}

// TestConfigService_Set_ExistingNestedKey_Persists: setting a known schema key
// updates it and survives a save/reload round-trip.
func TestConfigService_Set_ExistingNestedKey_Persists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	svc, err := NewConfigService(path)
	require.NoError(t, err)

	require.NoError(t, svc.Set("api.http_port", float64(9999)))
	require.NoError(t, svc.Save())

	reloaded, err := NewConfigService(path)
	require.NoError(t, err)
	got, err := reloaded.Get("api.http_port")
	require.NoError(t, err)
	assert.Equal(t, float64(9999), got)
}

// TestConfigService_Set_UnknownDeepKey_DroppedBySchema documents the real
// behavior: Set builds the nested map (exercising setNestedValue's
// create-submap path) but the value round-trips through the TYPED Config
// struct, which has no such field, so an unknown key is silently dropped — it
// succeeds without error yet does not persist.
func TestConfigService_Set_UnknownDeepKey_DroppedBySchema(t *testing.T) {
	svc, err := NewConfigService(filepath.Join(t.TempDir(), "c.json"))
	require.NoError(t, err)

	require.NoError(t, svc.Set("extensions.cache.maxEntries", float64(42)),
		"Set of an unknown nested key returns no error")
	_, err = svc.Get("extensions.cache.maxEntries")
	assert.Error(t, err, "an unknown key is dropped by the typed config schema, so Get cannot find it")
}
