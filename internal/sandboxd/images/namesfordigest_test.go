package images

import (
	"reflect"
	"testing"
)

func TestNamesForDigest_ReturnsEveryNameWithThatDigestSorted(t *testing.T) {
	root := t.TempDir()
	lock := Lock{Images: map[string]Entry{
		"zeta":  {Digest: "sha256:aa"},
		"alpha": {Digest: "sha256:aa"},
		"other": {Digest: "sha256:bb"},
	}}
	if err := writeLockFile(root, lock); err != nil {
		t.Fatal(err)
	}
	got, err := NamesForDigest(root, "sha256:aa")
	if err != nil || !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	none, err := NamesForDigest(root, "sha256:cc")
	if err != nil || len(none) != 0 {
		t.Fatalf("no match: %v, %v", none, err)
	}
}
