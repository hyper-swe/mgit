package staging

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkGuestRepo20K measures only the creation of the base commit on a
// freshly staged tree. Fixture generation is outside the timed region.
func BenchmarkGuestRepo20K(b *testing.B) {
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		root := b.TempDir()
		for d := 0; d < 100; d++ {
			dir := filepath.Join(root, fmt.Sprintf("pkg%03d", d))
			if err := os.Mkdir(dir, 0o750); err != nil {
				b.Fatal(err)
			}
			for f := 0; f < 200; f++ {
				name := fmt.Sprintf("source%03d", f)
				if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("%d:%d\n", d, f)), 0o600); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StartTimer()
		err := initGuestRepo(root)
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
	}
}
