package reviewbind

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// InfoSchema versions the machine-readable description of an installed map.
const InfoSchema = "mgit.review-map-info/v1"

const (
	defaultName  = "_default" // not a valid map id, so no map can take its name
	installHint  = "mgit sandbox review map install --file <map.json> --default"
	dirMode      = 0o700
	fileMode     = 0o600
	maxDefaultID = 128
)

// Info describes an installed, verified map. BindingSHA256 is the digest the
// record carries; MapSHA256 is the canonical hash of the whole record that the
// operator pinned. They are different values. It names no credential.
type Info struct {
	Schema         string `json:"schema"`
	ID             string `json:"id"`
	BindingSHA256  string `json:"binding_sha256"`
	ImageDigest    string `json:"image_digest"`
	MapSHA256      string `json:"map_sha256"`
	EgressPolicyID string `json:"egress_policy_id"`
}

func infoOf(m *reviewprep.Mapping) (Info, error) {
	h, err := CanonicalHash(*m)
	if err != nil {
		return Info{}, err
	}
	e := m.EgressPolicyID
	if e == "" {
		e = "none"
	}
	return Info{Schema: InfoSchema, ID: m.ID, BindingSHA256: m.BindingSHA256, ImageDigest: m.ImageDigest, MapSHA256: h, EgressPolicyID: e}, nil
}

// ensureDir creates d as an owner-only directory, or checks that an existing
// one is a plain directory that only its owner can write.
func ensureDir(d string, parents bool) error {
	fi, err := os.Lstat(d)
	switch {
	case err == nil:
		if !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
			return bad("directory " + d + " must be a plain directory only its owner can write; run: chmod 0700 " + d)
		}
		return nil
	case !os.IsNotExist(err):
		return err
	}
	if parents {
		return os.MkdirAll(d, dirMode)
	}
	return os.Mkdir(d, dirMode)
}

func mapsDir(hostRoot string) (string, error) {
	review := filepath.Join(hostRoot, "review")
	dir := filepath.Join(review, "maps")
	if err := ensureDir(hostRoot, true); err != nil {
		return "", err
	}
	if err := ensureDir(review, false); err != nil {
		return "", err
	}
	if err := ensureDir(dir, false); err != nil {
		return "", err
	}
	return dir, nil
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode) //nolint:gosec // path is built from a checked id under the operator's directory
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// replace writes data to path through a temporary file and a rename, so a
// reader sees the old pointer or the new one, never a partial one.
func replace(path string, data []byte) error {
	tmp := path + ".new"
	_ = os.Remove(tmp)
	if err := writeNew(tmp, data); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Install validates raw as a map record, writes it and its pinned canonical
// hash under hostRoot/review/maps with owner-only modes, and refuses an id that
// is already installed. What it writes is what LoadMap accepts: the result is
// loaded back before it is reported, and removed again if it does not load.
// With makeDefault the id also becomes the default map.
func Install(hostRoot string, raw []byte, makeDefault bool) (Info, error) {
	if len(raw) > maxMapBytes {
		return Info{}, bad("map record is too large")
	}
	var m reviewprep.Mapping
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Info{}, bad("map record is not a valid mgit.review-map/v1 record")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Info{}, bad("map record has bytes after the record")
	}
	if m.Schema != reviewprep.MapSchema || !idPattern.MatchString(m.ID) {
		return Info{}, bad("map record has the wrong schema or an id this build does not accept")
	}
	canon, err := json.Marshal(m)
	if err != nil {
		return Info{}, err
	}
	hash, err := CanonicalHashJSON(canon)
	if err != nil {
		return Info{}, err
	}
	dir, err := mapsDir(hostRoot)
	if err != nil {
		return Info{}, err
	}
	rec, pin := filepath.Join(dir, m.ID+".json"), filepath.Join(dir, m.ID+".sha256")
	if err := writeNew(rec, canon); err != nil {
		if os.IsExist(err) {
			return Info{}, bad("a map with this id is already installed; it is never overwritten")
		}
		return Info{}, err
	}
	if err := writeNew(pin, []byte(hash+"\n")); err != nil {
		_ = os.Remove(rec)
		return Info{}, err
	}
	loaded, err := LoadMap(hostRoot, m.ID)
	if err != nil {
		_ = os.Remove(rec)
		_ = os.Remove(pin)
		return Info{}, err
	}
	if makeDefault {
		sum := sha256.Sum256([]byte(m.ID))
		if err := replace(filepath.Join(dir, defaultName), []byte(m.ID+"\n")); err != nil {
			return Info{}, err
		}
		if err := replace(filepath.Join(dir, defaultName+".sha256"), []byte(hex.EncodeToString(sum[:])+"\n")); err != nil {
			return Info{}, err
		}
	}
	return infoOf(loaded)
}

// DefaultID returns the id of the default map. The pointer is pinned like a
// map, so an edited pointer is refused; nothing picks a map on its own.
func DefaultID(hostRoot string) (string, error) {
	dir := filepath.Join(hostRoot, "review", "maps")
	b, err := readPlain(filepath.Join(dir, defaultName), maxDefaultID)
	if err != nil {
		return "", bad("no default map is installed; install one with: " + installHint)
	}
	id := strings.TrimSpace(string(b))
	pin, err := readPlain(filepath.Join(dir, defaultName+".sha256"), 256)
	sum := sha256.Sum256([]byte(id))
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(pin)), hex.EncodeToString(sum[:])) || !idPattern.MatchString(id) {
		return "", bad("the default map pointer does not match its pin; reinstall with: " + installHint)
	}
	return id, nil
}

// Show verifies an installed map and describes it. An empty id means the
// default map.
func Show(hostRoot, id string) (Info, error) {
	if id == "" {
		var err error
		if id, err = DefaultID(hostRoot); err != nil {
			return Info{}, err
		}
	}
	m, err := LoadMap(hostRoot, id)
	if err != nil {
		return Info{}, err
	}
	return infoOf(m)
}
