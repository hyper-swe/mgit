package packaging

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A declared archive file list replaces GoReleaser's defaults. NOTICE must
// therefore be selected explicitly, not just exist in the checkout or in a
// build directory. Refs: MGIT-316
func TestGoreleaser_ArchivesShipNotice(t *testing.T) {
	_, archives, ok := strings.Cut(readRepoFile(t, ".goreleaser.yaml"), "\narchives:\n")
	if !ok {
		t.Fatal("release configuration declares no archives section")
	}
	// Limit the check to archives, not a similarly named file elsewhere.
	if end := regexp.MustCompile(`(?m)^[^ \t#\r\n]`).FindStringIndex(archives); end != nil {
		archives = archives[:end[0]]
	}
	blocks := strings.Split("\n"+archives, "\n  - id: ")[1:]
	if len(blocks) == 0 {
		t.Fatal("release configuration declares no archive ids")
	}
	for _, block := range blocks {
		id, body, _ := strings.Cut(block, "\n")
		_, files, ok := strings.Cut(body, "\n    files:\n")
		if !ok {
			t.Errorf("archive %q has no explicit files list for NOTICE", id)
			continue
		}
		if end := regexp.MustCompile(`(?m)^    [^ \t#\r\n]`).FindStringIndex(files); end != nil {
			files = files[:end[0]]
		}
		if !strings.Contains("\n"+files, "\n      - NOTICE*\n") {
			t.Errorf("archive %q does not select root NOTICE* in its files list", id)
		}
	}
}

// TestArchives_ShipTheNotice checks the shipped artifact, not its build
// directory. make verify-archive supplies the real tar.gz and zip files.
func TestArchives_ShipTheNotice(t *testing.T) {
	for _, archive := range builtArchives(t) {
		t.Run(filepath.Base(archive), func(t *testing.T) {
			names, err := archiveEntries(archive)
			if err != nil {
				t.Fatalf("read archive: %v", err)
			}
			if !contains(names, "NOTICE") {
				t.Errorf("NOTICE is missing from the archive; third-party notices must accompany the download")
			}
		})
	}
}
