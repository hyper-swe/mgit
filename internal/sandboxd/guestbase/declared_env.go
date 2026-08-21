package guestbase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// declaredEnvFile is where a composed base records the PATH its image
// declared, relative to the base root. It must stay identical to
// guest.DeclaredGuestEnvFile — the guest reads exactly this file — and a test
// on each side pins the format they share. Refs: MGIT-152
const declaredEnvFile = "etc/mgit/guest-env"

// maxConfigBlob bounds the image config we will read. A config is a small JSON
// document; anything enormous is not one.
const maxConfigBlob = 1 << 20

// imageConfigDoc is the subset of an OCI image config mgit reads.
type imageConfigDoc struct {
	Config imageConfigInner `json:"config"`
}

// imageConfigInner carries the declared environment.
type imageConfigInner struct {
	Env []string `json:"Env"`
}

// pathFromImageConfig returns the PATH an image declared, or "".
//
// ONLY PATH is taken. An image config carries a whole environment, and
// honoring it wholesale would let an untrusted base set LD_PRELOAD or
// LD_LIBRARY_PATH for every command mgit runs on a user's behalf. The problem
// being solved is that a toolchain outside the distro's default directories is
// invisible, and PATH is the whole of that problem. Refs: MGIT-152
func pathFromImageConfig(doc imageConfigDoc) string {
	out := ""
	for _, e := range doc.Config.Env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			out = v // last wins, as in any environment
		}
	}
	return out
}

// renderDeclaredEnv renders the declaration the guest will read back.
func renderDeclaredEnv(path string) string {
	if path == "" {
		return ""
	}
	return "PATH=" + path + "\n"
}

// writeDeclaredEnv records the image's declared PATH inside the composed base.
//
// It goes in the TREE rather than in images.lock deliberately: the base's
// content digest already covers every byte of the tree, so tampering with the
// declared PATH is exactly as detectable as tampering with a binary — with no
// new signing input and no change to the lock format. Refs: MGIT-152, SEC-12
func writeDeclaredEnv(destDir, declaredPath string) error {
	body := renderDeclaredEnv(declaredPath)
	if body == "" {
		return nil
	}
	full := filepath.Join(destDir, filepath.FromSlash(declaredEnvFile))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return fmt.Errorf("guest base: create %s: %w", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		return fmt.Errorf("guest base: write %s: %w", declaredEnvFile, err)
	}
	return nil
}

// fetchImageConfig reads and parses an image's config blob.
func (c *client) fetchImageConfig(ctx context.Context, ref Ref, cfg descriptor) (imageConfigDoc, error) {
	var doc imageConfigDoc
	if cfg.Digest == "" {
		return doc, nil
	}
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", c.scheme, ref.Registry, ref.Repository, cfg.Digest)
	body, err := c.get(ctx, url, ref, []string{cfg.MediaType, "*/*"})
	if err != nil {
		return doc, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, maxConfigBlob))
	if err != nil {
		return doc, fmt.Errorf("guest base: read image config: %w", err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("guest base: parse image config: %w", err)
	}
	return doc, nil
}
