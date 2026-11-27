package git

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type unreadableSnapshotObject struct {
	plumbing.EncodedObject
	err error
}

func (o unreadableSnapshotObject) Reader() (io.ReadCloser, error) { return nil, o.err }

func TestWriteSnapshotFilePreservesDestinationOnFailure(t *testing.T) {
	mem := &plumbing.MemoryObject{}
	mem.SetType(plumbing.BlobObject)
	if _, err := mem.Write([]byte("restored")); err != nil {
		t.Fatal(err)
	}
	var blob object.Blob
	if err := blob.Decode(mem); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, entry, obstacle string }{
		{"parent is a file", "occupied/child", "occupied"},
		{"destination is a directory", "occupied", "occupied/marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			marker := filepath.Join(dest, tc.obstacle)
			if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			err := writeSnapshotFile(dest, object.NewFile(tc.entry, filemode.Regular, &blob))
			var pathErr *fs.PathError
			if !errors.As(err, &pathErr) {
				t.Fatalf("want filesystem error, got %v", err)
			}
			if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
				t.Fatalf("existing content = %q, err = %v", got, err)
			}
		})
	}
	t.Run("blob read failure", func(t *testing.T) {
		dest := t.TempDir()
		path := filepath.Join(dest, "existing")
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("blob unavailable")
		var bad object.Blob
		if err := bad.Decode(unreadableSnapshotObject{mem, cause}); err != nil {
			t.Fatal(err)
		}
		if err := writeSnapshotFile(dest, object.NewFile("existing", filemode.Regular, &bad)); !errors.Is(err, cause) {
			t.Fatalf("want blob error, got %v", err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "keep" {
			t.Fatalf("existing content = %q, err = %v", got, err)
		}
	})
	t.Run("executable file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires Unix execute bits")
		}
		dest := t.TempDir()
		if err := writeSnapshotFile(dest, object.NewFile("bin/tool", filemode.Executable, &blob)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dest, "bin", "tool")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0700 {
			t.Errorf("permissions = %o, want 700", perm)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "restored" {
			t.Fatalf("restored content = %q, err = %v", got, err)
		}
	})
}
