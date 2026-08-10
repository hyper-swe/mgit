//go:build !(linux || darwin)

package artifactexport

// readRecordedStat has nothing to read on this platform: the virtio-fs
// backends whose shares keep a stat record are linux- and macOS-hosted, so an
// export here always takes the mode from the host's own permission bits.
// Refs: MGIT-81
func readRecordedStat(string) string { return "" }
