package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/reviewbind"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

func TestReviewBindings_UnconfiguredDaemonStillUsesTheDefaultCredentialRefusal(t *testing.T) {
	b, err := reviewBindings(-1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, isDefault := b.(reviewsvc.NoBindings); isDefault {
		t.Fatal("the daemon must attach the map-aware binding, which itself refuses what it cannot provide")
	}
	if reflect.TypeOf(b) != reflect.TypeOf(reviewbind.New(reviewbind.Config{})) {
		t.Fatalf("unexpected binding type %T", b)
	}
}

func TestReviewBindings_ACredentialFDNeedsARef(t *testing.T) {
	if _, err := reviewBindings(5, "", nil); err == nil || !strings.Contains(err.Error(), "ref") {
		t.Fatalf("an fd without a ref must be refused: %v", err)
	}
}

func TestReviewBindings_ABadFDIsRefusedWithoutEchoingAnything(t *testing.T) {
	if _, err := reviewBindings(987654, "review-key", nil); err == nil {
		t.Fatal("an fd that is not open was accepted")
	}
}
