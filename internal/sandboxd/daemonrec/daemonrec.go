// Package daemonrec is the host-wide record of running sandbox daemons.
//
// From MGIT-185 and MGIT-191: six daemons from long-deleted or forgotten
// temp-directory repositories were alive on one host for eleven to thirteen
// days, and the only way to see them was ps. Each daemon now writes a small
// record beside its socket while it runs and removes it when it exits, so a
// host-wide view — `mgit sandbox daemons`, and doctor's daemons/host row —
// can name every daemon with its root, age and what is wrong with it, and
// stop one by pid and root rather than by a blanket pkill. Refs: MGIT-191, MGIT-185
package daemonrec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileName is the record's name beside the daemon's socket.
const FileName = "daemon.json"

// Record is what one daemon says about itself.
type Record struct {
	PID       int       `json:"pid"`
	RepoRoot  string    `json:"repo_root"`
	HostRoot  string    `json:"host_root,omitempty"`
	Socket    string    `json:"socket"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"version,omitempty"`
}

// Status is what an operator needs to know about a recorded daemon.
type Status struct {
	Alive    bool          // the pid answers signal 0
	RootGone bool          // the repository root no longer exists
	TempRoot bool          // the root lives under a temp directory (an e2e or pair run)
	Age      time.Duration // since StartedAt
}

// Leaked reports a daemon nobody meant to keep: its root is gone, or it has
// served a temp-directory root for longer than a run could plausibly need.
// A dead pid leaks nothing; its record is merely stale.
func (s Status) Leaked(tempGrace time.Duration) bool {
	if !s.Alive {
		return false
	}
	return s.RootGone || (s.TempRoot && s.Age > tempGrace)
}

// Listed is a record with its status.
type Listed struct {
	Record Record
	Status Status
}

// Write records the daemon beside its socket, atomically.
func Write(rec Record) error {
	path := filepath.Join(filepath.Dir(rec.Socket), FileName)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon record: encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("daemon record: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("daemon record: publish: %w", err)
	}
	return nil
}

// Read loads one record.
func Read(path string) (Record, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a record beside a daemon socket, named by the caller
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("daemon record %s: %w", path, err)
	}
	return rec, nil
}

// Remove deletes the record beside the socket; a missing record is fine.
func Remove(socket string) error {
	err := os.Remove(filepath.Join(filepath.Dir(socket), FileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon record: remove: %w", err)
	}
	return nil
}

// List reads every record one level under base (one directory per
// repository), in path order. Records that cannot be read are named in
// problems rather than dropped: a daemon nobody can see is the whole
// incident. Refs: R-H300
func List(base string) (recs []Record, problems []string, err error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read runtime base %s: %w", base, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(base, e.Name(), FileName)
		rec, rerr := Read(path)
		if errors.Is(rerr, os.ErrNotExist) {
			continue
		}
		if rerr != nil {
			problems = append(problems, rerr.Error())
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].RepoRoot < recs[j].RepoRoot })
	return recs, problems, nil
}

// Classify judges one record at a given moment. alive answers whether a pid
// is running; tempRoots are the directories under which a root counts as a
// temp-directory repository.
func Classify(rec Record, now time.Time, alive func(int) bool, tempRoots []string) Status {
	st := Status{Alive: alive(rec.PID), Age: now.Sub(rec.StartedAt).Truncate(time.Second)}
	if _, err := os.Stat(rec.RepoRoot); err != nil {
		st.RootGone = true
	}
	for _, t := range tempRoots {
		if t != "" && (rec.RepoRoot == t || strings.HasPrefix(rec.RepoRoot, strings.TrimSuffix(t, "/")+"/")) {
			st.TempRoot = true
			break
		}
	}
	return st
}
