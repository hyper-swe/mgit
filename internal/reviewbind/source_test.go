package reviewbind

import (
	"context"
	"strings"
	"testing"
)

func TestStaticSource_HandsOutACopyPerRunAndOnlyForItsRef(t *testing.T) {
	src, err := StaticSource("review-key", strings.NewReader("s3cr3t-value\n"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := src(context.Background(), "review-key")
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Take()
		if err != nil || string(b) != "s3cr3t-value" {
			t.Fatalf("run %d got %q, %v", i, b, err)
		}
		Wipe(b)
	}
	if _, err := src(context.Background(), "other-ref"); err == nil {
		t.Fatal("a credential was handed out under a ref it was not given for")
	}
}

func TestStaticSource_RefusesAnEmptyOrOversizeSecret(t *testing.T) {
	if _, err := StaticSource("k", strings.NewReader("")); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := StaticSource("k", strings.NewReader(strings.Repeat("x", MaxSecret+1))); err == nil {
		t.Fatal("oversize secret accepted")
	}
	if _, err := StaticSource("bad ref", strings.NewReader("x")); err == nil {
		t.Fatal("malformed ref accepted")
	}
}
