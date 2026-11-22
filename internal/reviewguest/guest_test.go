package reviewguest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const img = "reviewer@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeMgr struct {
	launched []model.SandboxLaunchOptions
	execs    []model.ExecRequest
	removed  []string
	backend  string
	res      *model.ExecResult
	err      error
}

func (f *fakeMgr) Launch(_ context.Context, o model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	f.launched = append(f.launched, o)
	if f.err != nil {
		return nil, f.err
	}
	return &model.SandboxInfo{ID: "sb1", Backend: f.backend, WorktreePath: o.WorktreePath, NetworkMode: o.Network.Mode}, nil
}
func (f *fakeMgr) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (f *fakeMgr) Exec(_ context.Context, _ string, r model.ExecRequest) (*model.ExecResult, error) {
	f.execs = append(f.execs, r)
	if f.res == nil {
		return &model.ExecResult{}, nil
	}
	return f.res, nil
}
func (f *fakeMgr) Stop(context.Context, string, bool) error { return nil }
func (f *fakeMgr) Remove(_ context.Context, id string, _ bool) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeMgr) Resolve(context.Context, string) (*model.SandboxInfo, error) { return nil, nil }

func spec(t *testing.T) Spec {
	t.Helper()
	d := filepath.Join(t.TempDir(), "lease")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return Spec{Backend: model.BackendKVM, LeaseDir: d, ImageRef: img, TaskID: "REV-1", UID: 65534, GID: 65534}
}

func code(t *testing.T, err error) string {
	t.Helper()
	var e *reviewprep.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *reviewprep.Error, got %T %v", err, err)
	}
	return e.Code
}

func TestProvision_LaunchesTheLeaseDirWithNoNetworkAndNoRepo(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s := spec(t)
	g, err := Provision(context.Background(), m, s)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "sb1" || len(m.launched) != 1 {
		t.Fatalf("id=%q launches=%d", g.ID, len(m.launched))
	}
	o := m.launched[0]
	if o.WorktreePath != s.LeaseDir || o.ImageRef != img || o.Network.Mode != model.NetworkModeNone || !o.NoGuestRepo || o.ConfineAgent || len(o.PublishPorts) != 0 {
		t.Fatalf("launch options %+v", o)
	}
}

func TestProvision_RefusesBackendsThatShareTheHostDirectoryLive(t *testing.T) {
	for _, b := range []string{model.BackendLibkrun, model.BackendVZF, model.BackendContainer, model.BackendHyperV, ""} {
		m := &fakeMgr{backend: b}
		s := spec(t)
		s.Backend = b
		_, err := Provision(context.Background(), m, s)
		if err == nil || code(t, err) != reviewprep.CodePolicyUnenforcable {
			t.Fatalf("backend %q: err=%v", b, err)
		}
		if len(m.launched) != 0 {
			t.Fatalf("backend %q: launched before refusing", b)
		}
	}
}

func TestProvision_RefusesAndRemovesWhenTheLaunchedBackendIsNotTheDeclaredOne(t *testing.T) {
	m := &fakeMgr{backend: model.BackendLibkrun}
	_, err := Provision(context.Background(), m, spec(t))
	if err == nil || code(t, err) != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(m.removed, []string{"sb1"}) {
		t.Fatalf("removed=%v", m.removed)
	}
}

func TestProvision_RefusesALeaseDirThatIsNotASealedDirectory(t *testing.T) {
	s := spec(t)
	file := filepath.Join(filepath.Dir(s.LeaseDir), "f")
	if err := os.WriteFile(file, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(s.LeaseDir), "l")
	if err := os.Symlink(s.LeaseDir, link); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"relative": "lease", "dot": ".", "missing": filepath.Join(s.LeaseDir, "nope"), "file": file, "symlink": link} {
		m := &fakeMgr{backend: model.BackendKVM}
		x := s
		x.LeaseDir = p
		_, err := Provision(context.Background(), m, x)
		if err == nil || code(t, err) != reviewprep.CodeBundleInvalid || len(m.launched) != 0 {
			t.Fatalf("%s: err=%v launches=%d", name, err, len(m.launched))
		}
	}
}

func TestProvision_RefusesAnImageThatIsNotPinned(t *testing.T) {
	s := spec(t)
	s.ImageRef = "reviewer:latest"
	m := &fakeMgr{backend: model.BackendKVM}
	_, err := Provision(context.Background(), m, s)
	if err == nil || code(t, err) != reviewprep.CodeImageMismatch || len(m.launched) != 0 {
		t.Fatalf("err=%v", err)
	}
}

func TestProvision_ReportsALaunchFailureWithoutRemovingAnything(t *testing.T) {
	m := &fakeMgr{err: errors.New("boom")}
	if _, err := Provision(context.Background(), m, spec(t)); err == nil || len(m.removed) != 0 {
		t.Fatalf("err=%v removed=%v", err, m.removed)
	}
}

func TestRun_GoesThroughTheLauncherAsRootWithTheDigestAndNoEnvironment(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g, _ := Provision(context.Background(), m, spec(t))
	_, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/usr/bin/reviewer", Argv: []string{"--json", "-x"}})
	if err != nil {
		t.Fatal(err)
	}
	r := m.execs[0]
	want := []string{"/sbin/mgit-guest", "review-exec", "--sha256", sha, "--uid", "65534", "--gid", "65534", "--dir", g.LeaseDir, "--", "/usr/bin/reviewer", "--json", "-x"}
	if !reflect.DeepEqual(r.Command, want) {
		t.Fatalf("command %q", r.Command)
	}
	if !r.AsRoot || len(r.Env) != 0 || r.Dir != g.LeaseDir {
		t.Fatalf("exec %+v", r)
	}
}

func TestRun_RefusesADigestThatIsNotHex(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g, _ := Provision(context.Background(), m, spec(t))
	for _, e := range []Entry{{SHA256: "xyz", Path: "/a"}, {SHA256: sha, Path: "relative"}, {SHA256: sha, Path: ""}} {
		if _, err := g.Run(context.Background(), e); err == nil || code(t, err) != reviewprep.CodeMappingMismatch {
			t.Fatalf("%+v: err=%v", e, err)
		}
	}
	if len(m.execs) != 0 {
		t.Fatal("exec reached the guest")
	}
}

func TestRun_ADeclinedLaunchComesBackAsTheRefusalEnvelope(t *testing.T) {
	env, _ := json.Marshal(map[string]any{"schema": "mgit.review-error/v1", "code": "IMAGE_MISMATCH", "phase": "launch", "reviewer_started": false, "detail": "d"})
	m := &fakeMgr{backend: model.BackendKVM, res: &model.ExecResult{ExitCode: 70, Stderr: append(env, '\n')}}
	g, _ := Provision(context.Background(), m, spec(t))
	_, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"})
	if err == nil || code(t, err) != "IMAGE_MISMATCH" {
		t.Fatalf("err=%v", err)
	}
}

func TestRun_AnyOtherExitIsTheReviewersOwn(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM, res: &model.ExecResult{ExitCode: 3, Stdout: []byte("out")}}
	g, _ := Provision(context.Background(), m, spec(t))
	res, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"})
	if err != nil || res.ExitCode != 3 || string(res.Stdout) != "out" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestClose_RemovesTheSandboxForced(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g, _ := Provision(context.Background(), m, spec(t))
	if err := g.Close(context.Background()); err != nil || !reflect.DeepEqual(m.removed, []string{"sb1"}) {
		t.Fatalf("err=%v removed=%v", err, m.removed)
	}
}

func TestProvision_RefusesAnIdentityThatIsRootOrUnset(t *testing.T) {
	for _, id := range [][2]int{{0, 0}, {0, 5}, {5, 0}, {-1, 5}, {5, -1}} {
		m := &fakeMgr{backend: model.BackendKVM}
		s := spec(t)
		s.UID, s.GID = id[0], id[1]
		_, err := Provision(context.Background(), m, s)
		if err == nil || code(t, err) != reviewprep.CodeMappingMismatch || len(m.launched) != 0 {
			t.Fatalf("%v: err=%v launches=%d", id, err, len(m.launched))
		}
	}
}
