package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/store/lock"
)

// `locks.timeout_seconds` was promised by REQUIREMENTS.md (FR-4.7, NFR-3.5)
// and did not exist: the wait was a compile-time constant. These tests pin the
// two properties that make the knob safe to ship — the default is exactly the
// old behavior, and a hostile value cannot turn a contended lock into an
// unbounded hang. Refs: FR-4.7, NFR-3.5, MGIT-120

func TestLockTimeoutFromConfig_ValueVariants_ResolveAsSpecified(t *testing.T) {
	tests := []struct {
		name    string
		content string // "" means: write no config file at all
		want    time.Duration
	}{
		{
			name:    "no_config_file_keeps_default",
			content: "",
			want:    lock.DefaultTimeout,
		},
		{
			name:    "config_without_locks_section_keeps_default",
			content: `{"project":{"prefix":"MGIT"}}`,
			want:    lock.DefaultTimeout,
		},
		{
			name:    "explicit_value_is_honored",
			content: `{"locks":{"timeout_seconds":5}}`,
			want:    5 * time.Second,
		},
		{
			name:    "zero_means_unset_and_keeps_default",
			content: `{"locks":{"timeout_seconds":0}}`,
			want:    lock.DefaultTimeout,
		},
		{
			name:    "negative_is_rejected_and_keeps_default",
			content: `{"locks":{"timeout_seconds":-9}}`,
			want:    lock.DefaultTimeout,
		},
		{
			name:    "absurd_value_is_clamped",
			content: `{"locks":{"timeout_seconds":999999999}}`,
			want:    MaxLockTimeout,
		},
		{
			name:    "malformed_config_keeps_default",
			content: `{"locks":{"timeout_seconds":`,
			want:    lock.DefaultTimeout,
		},
		{
			name:    "wrong_type_keeps_default",
			content: `{"locks":{"timeout_seconds":"soon"}}`,
			want:    lock.DefaultTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tt.content != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			}
			assert.Equal(t, tt.want, LockTimeoutFromConfig(path))
		})
	}
}

// TestDefaultConfig_LockTimeout_MatchesHistoricalConstant proves the shipped
// default did not move when the knob was introduced. Refs: MGIT-120
func TestDefaultConfig_LockTimeout_MatchesHistoricalConstant(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, int(lock.DefaultTimeout/time.Second), cfg.Locks.TimeoutSeconds)
	assert.Equal(t, 30, cfg.Locks.TimeoutSeconds, "the historical wait is 30s")
}

// TestConfigService_Set_ScalarValues_RoundTripThroughStrings covers the CLI
// path: `mgit config set` hands every value over as a string, so a numeric or
// boolean setting was unsettable before the coercion retry. Refs: FR-13, MGIT-120
func TestConfigService_Set_ScalarValues_RoundTripThroughStrings(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
		check   func(t *testing.T, c Config)
	}{
		{
			name:  "int_key_from_string",
			key:   "locks.timeout_seconds",
			value: "7",
			check: func(t *testing.T, c Config) { assert.Equal(t, 7, c.Locks.TimeoutSeconds) },
		},
		{
			name:  "bool_key_from_string",
			key:   "git.auto_stage",
			value: "true",
			check: func(t *testing.T, c Config) { assert.True(t, c.Git.AutoStage) },
		},
		{
			name:  "string_key_is_left_alone",
			key:   "logging.level",
			value: "debug",
			check: func(t *testing.T, c Config) { assert.Equal(t, "debug", c.Logging.Level) },
		},
		{
			name:    "non_numeric_value_for_int_key_is_still_refused",
			key:     "locks.timeout_seconds",
			value:   "soon",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			svc, err := NewConfigService(path)
			require.NoError(t, err)

			err = svc.Set(tt.key, tt.value)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NoError(t, svc.Save())

			reloaded, err := NewConfigService(path)
			require.NoError(t, err)
			tt.check(t, reloaded.GetAll())
		})
	}
}

// TestLockTimeoutFromConfig_SetThenRead_IsHonored is the end-to-end shape of
// the knob: what `mgit config set` writes is what the next process waits.
// Refs: FR-4.7, MGIT-120
func TestLockTimeoutFromConfig_SetThenRead_IsHonored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	svc, err := NewConfigService(path)
	require.NoError(t, err)
	require.NoError(t, svc.Set("locks.timeout_seconds", "3"))
	require.NoError(t, svc.Save())

	assert.Equal(t, 3*time.Second, LockTimeoutFromConfig(path))
}
