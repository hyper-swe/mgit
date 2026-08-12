// Package hostmem tests verify the host physical-memory probe that resolves
// the FR-17.26 aggregate memory ceiling from host policy. The parser tests are
// deliberately platform-independent: the Linux probe reads /proc/meminfo, which
// does not exist on a macOS developer machine, so the parse and file-read paths
// are exercised against real captured meminfo content on every platform rather
// than only on Linux. Refs: FR-17.26, MGIT-98
package hostmem

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realMeminfo is the head of an actual /proc/meminfo from a 64-bit Linux host
// (16 GiB, kernel 6.x). MemTotal is deliberately NOT the first line and the
// file carries the trailing "kB" unit the kernel always emits.
const realMeminfo = `MemTotal:       16311456 kB
MemFree:         2196772 kB
MemAvailable:   11748456 kB
Buffers:          262144 kB
Cached:          9033728 kB
SwapCached:            0 kB
Active:          6142084 kB
Inactive:        6528340 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
`

// TestParseMemTotal_RealProcMeminfo_ReturnsPhysicalBytes pins the unit
// conversion. Getting this wrong by a factor of 1024 yields a
// plausible-but-wrong ceiling, which is worse than no ceiling at all: it would
// silently admit (or refuse) a thousandfold wrong fleet. Refs: FR-17.26
func TestParseMemTotal_RealProcMeminfo_ReturnsPhysicalBytes(t *testing.T) {
	tests := []struct {
		name      string
		meminfo   string
		want      uint64
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "real_16gib_host",
			meminfo: realMeminfo,
			want:    16311456 * 1024,
		},
		{
			name:    "memtotal_is_first_line",
			meminfo: "MemTotal:        1048576 kB\nMemFree:          524288 kB\n",
			want:    1048576 * 1024,
		},
		{
			name:    "no_trailing_newline",
			meminfo: "MemTotal:        2097152 kB",
			want:    2097152 * 1024,
		},
		{
			name:      "missing_memtotal_line",
			meminfo:   "MemFree:          524288 kB\nCached:           131072 kB\n",
			wantErr:   true,
			errSubstr: "MemTotal",
		},
		{
			name:      "empty_file",
			meminfo:   "",
			wantErr:   true,
			errSubstr: "MemTotal",
		},
		{
			// A unit mgit does not understand must fail closed rather than be
			// assumed to be kB: a wrong unit is a wrong ceiling.
			name:      "unexpected_unit",
			meminfo:   "MemTotal:       16311456 MB\n",
			wantErr:   true,
			errSubstr: "unit",
		},
		{
			name:      "missing_unit",
			meminfo:   "MemTotal:       16311456\n",
			wantErr:   true,
			errSubstr: "unit",
		},
		{
			name:      "non_numeric_value",
			meminfo:   "MemTotal:       lots kB\n",
			wantErr:   true,
			errSubstr: "MemTotal",
		},
		{
			name:      "zero_is_not_a_believable_host",
			meminfo:   "MemTotal:              0 kB\n",
			wantErr:   true,
			errSubstr: "0",
		},
		{
			// A value that would overflow when scaled to bytes is refused
			// rather than silently wrapping into a small (or negative) size.
			name:      "value_overflows_a_byte_count",
			meminfo:   "MemTotal:    18446744073709551615 kB\n",
			wantErr:   true,
			errSubstr: "overflow",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMemTotal(strings.NewReader(tt.meminfo))
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errSubstr)
				assert.Zero(t, got, "a failed parse must never hand back a usable number")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// errReader fails mid-stream, standing in for a /proc read that faults.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("simulated /proc read fault") }

// TestParseMemTotal_ReadFails_ReportsTheReadError verifies a faulting read is
// reported rather than mistaken for a file with no MemTotal line — the caller
// fails closed either way, but the operator needs the real reason.
// Refs: FR-17.26
func TestParseMemTotal_ReadFails_ReportsTheReadError(t *testing.T) {
	got, err := parseMemTotal(errReader{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read meminfo")
	assert.Zero(t, got)
}

// TestTotalBytesFromMeminfo_ReadsTheFile exercises the Linux probe's file-read
// path on any platform by pointing it at captured meminfo content.
// Refs: FR-17.26
func TestTotalBytesFromMeminfo_ReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	require.NoError(t, os.WriteFile(path, []byte(realMeminfo), 0o600))

	got, err := totalBytesFromMeminfo(path)

	require.NoError(t, err)
	assert.Equal(t, uint64(16311456)*1024, got)
}

// TestTotalBytesFromMeminfo_Unreadable_ReturnsError verifies a missing
// /proc/meminfo is an ERROR, not a zero. The caller fails closed to a
// conservative absolute on error; a silent zero would read as "no ceiling".
// Refs: FR-17.26
func TestTotalBytesFromMeminfo_Unreadable_ReturnsError(t *testing.T) {
	got, err := totalBytesFromMeminfo(filepath.Join(t.TempDir(), "absent"))

	require.Error(t, err)
	assert.Zero(t, got)
}

// TestTotalBytes_OnThisHost_ReportsPlausiblePhysicalMemory runs the REAL
// platform probe. On a supported platform it must report a believable physical
// memory size; on an unsupported one it must report an error rather than a
// number the caller would trust. Refs: FR-17.26
func TestTotalBytes_OnThisHost_ReportsPlausiblePhysicalMemory(t *testing.T) {
	got, err := TotalBytes()

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		require.Error(t, err, "an unprobeable platform must say so, never guess")
		assert.Zero(t, got)
		return
	}

	require.NoError(t, err)
	assert.GreaterOrEqual(t, got, uint64(256)<<20,
		"no host mgit can run on has under 256 MiB of RAM; a smaller answer means the probe is wrong")
	assert.Less(t, got, uint64(64)<<40,
		"64 TiB would mean the unit conversion is off, not that the host is enormous")
	t.Logf("host physical memory probe: %d bytes (%d MiB) on %s/%s",
		got, got>>20, runtime.GOOS, runtime.GOARCH)
}
