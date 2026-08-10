package artifactexport

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseRecordedStat_RealBackendValues_YieldThePermissionBits(t *testing.T) {
	tests := []struct {
		name        string
		value       string
		want        fs.FileMode
		wroteRecord bool
	}{
		{
			// The exact bytes libkrun's macOS virtio-fs wrote for a guest
			// 0755 regular file, measured on 2026-08-10 (MGIT-81).
			name: "regular_file_with_type_bits", value: "0:0:0100755",
			want: 0o755, wroteRecord: true,
		},
		{
			// The exact bytes it wrote for a guest 0755 directory: no type
			// bits at all, so the parser must not require them.
			name: "directory_without_type_bits", value: "0:0:0755",
			want: 0o755, wroteRecord: true,
		},
		{name: "plain_data_file", value: "0:0:0100644", want: 0o644, wroteRecord: true},
		{name: "private_file", value: "0:0:0100600", want: 0o600, wroteRecord: true},
		{name: "non_root_ids", value: "1000:1000:0100755", want: 0o755, wroteRecord: true},
		{
			// setuid/setgid/sticky are DROPPED: the export has never carried
			// them (it uses Go's Perm()), and a guest-influenced record is not
			// where mgit would start.
			name: "setuid_is_masked_off", value: "0:0:0104755", want: 0o755, wroteRecord: true,
		},
		{name: "setgid_is_masked_off", value: "0:0:0102755", want: 0o755, wroteRecord: true},
		{name: "sticky_is_masked_off", value: "0:0:0101777", want: 0o777, wroteRecord: true},
		{name: "empty", value: "", want: 0, wroteRecord: false},
		{name: "too_few_fields", value: "0:0", want: 0, wroteRecord: false},
		{name: "too_many_fields", value: "0:0:0755:extra", want: 0, wroteRecord: false},
		{name: "not_a_number", value: "0:0:rwxr-xr-x", want: 0, wroteRecord: false},
		{name: "negative", value: "0:0:-0755", want: 0, wroteRecord: false},
		{name: "out_of_range", value: "0:0:07777777777777777777777", want: 0, wroteRecord: false},
		{name: "junk", value: "hello", want: 0, wroteRecord: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRecordedStat(tt.value)
			assert.Equal(t, tt.wroteRecord, ok, "recognized?")
			if tt.wroteRecord {
				assert.Equal(t, tt.want, got, "permission bits")
			}
		})
	}
}
