package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/state"
)

// A "build" is a tiny shell script that prints its version, so the real probe (running `<binary> version`) is exercised.
func build(version string) []byte {
	return []byte("#!/bin/sh\ncase \"$1\" in selftest) echo ok;; *) echo " + version + ";; esac\n")
}

// brokenOnStart prints its version but cannot start up (a self-test fails): it would be dead on arrival and could not roll itself back.
func brokenOnStart(version string) []byte {
	return []byte("#!/bin/sh\ncase \"$1\" in selftest) echo \"cannot reach the platform\" >&2; exit 1;; *) echo " + version + ";; esac\n")
}
func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type rig struct {
	t      *testing.T
	dir    string
	exe    string
	stateD string
	st     *state.State
	srv    *httptest.Server
	served []byte
	status int
	up     Updater
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), status: 200, served: build("0.3.0")}
	r.exe = filepath.Join(r.dir, "nuit-cloud-agent")
	if err := os.WriteFile(r.exe, build("0.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.stateD = filepath.Join(r.dir, "state")
	r.st = state.New(r.stateD)
	r.st.PlatformURL, r.st.MachineID, r.st.Credential = "https://p", "m", "c"
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/agent/download/nuit-cloud-agent-linux-"+runtime.GOARCH {
			t.Errorf("asked for %q", req.URL.Path)
		}
		w.WriteHeader(r.status)
		_, _ = w.Write(r.served)
	}))
	t.Cleanup(r.srv.Close)
	r.up = Updater{BaseURL: r.srv.URL, ExePath: r.exe, StateDir: r.stateD, Version: "0.2.0", Arch: runtime.GOARCH, State: r.st, Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }}
	return r
}

func (r *rig) params(version string, body []byte) Params {
	return Params{Version: version, SHA256: map[string]string{runtime.GOARCH: sum(body)}}
}

func (r *rig) exeContent() string { b, _ := os.ReadFile(r.exe); return string(b) }

func (r *rig) leftovers() []string {
	entries, _ := os.ReadDir(r.dir)
	var extra []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nuit-cloud-agent.new-") {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

func TestAGoodUpdateReplacesTheBinaryKeepsThePreviousOneAndIsPending(t *testing.T) {
	r := newRig(t)
	out, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served))
	if err != nil || !out.Changed || out.From != "0.2.0" || out.To != "0.3.0" {
		t.Fatalf("%+v %v", out, err)
	}
	if r.exeContent() != string(build("0.3.0")) {
		t.Error("the running binary was not replaced")
	}
	prev, _ := os.ReadFile(r.up.PreviousPath())
	if string(prev) != string(build("0.2.0")) {
		t.Errorf("the previous binary was not kept: %q", prev)
	}
	info, _ := os.Stat(r.exe)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	p := r.st.Pending()
	if p == nil || p.From != "0.2.0" || p.To != "0.3.0" || p.Previous != r.up.PreviousPath() {
		t.Errorf("pending: %+v", p)
	}
	if len(r.leftovers()) != 0 {
		t.Errorf("staging files left behind: %v", r.leftovers())
	}
}

func TestATamperedDownloadIsRefusedAndNothingChanges(t *testing.T) {
	r := newRig(t)
	good := r.params("0.3.0", r.served)
	r.served = []byte("#!/bin/sh\necho 0.3.0\nrm -rf /\n") // what an attacker swapped in: not the build that was signed
	_, err := r.up.Apply(context.Background(), good)
	if err == nil || !strings.Contains(err.Error(), "does not match the checksum the platform signed") {
		t.Fatalf("got %v", err)
	}
	if r.exeContent() != string(build("0.2.0")) || r.st.Pending() != nil || len(r.leftovers()) != 0 {
		t.Error("a refused update must change nothing and leave nothing")
	}
	if _, err := os.Stat(r.up.PreviousPath()); err == nil {
		t.Error("no previous copy should be made before the download is trusted")
	}
}

func TestABuildThatSaysItIsAnotherVersionOrWillNotRunIsRefused(t *testing.T) {
	r := newRig(t)
	r.served = build("9.9.9")
	_, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served)) // checksum matches, but it is not the promised version
	if err == nil || !strings.Contains(err.Error(), `says it is version "9.9.9"`) {
		t.Fatalf("got %v", err)
	}
	if r.exeContent() != string(build("0.2.0")) || r.st.Pending() != nil {
		t.Error("nothing may change")
	}

	r = newRig(t)
	r.served = []byte("not a program") // right checksum, but cannot run on this machine (the wrong CPU would look like this)
	_, err = r.up.Apply(context.Background(), r.params("0.3.0", r.served))
	if err == nil || !strings.Contains(err.Error(), "does not run on this machine") {
		t.Fatalf("got %v", err)
	}
	if r.exeContent() != string(build("0.2.0")) {
		t.Error("nothing may change")
	}
}

func TestABuildThatPrintsItsVersionButFailsItsSelfTestIsRefusedBeforeAnythingIsReplaced(t *testing.T) {
	r := newRig(t)
	r.served = brokenOnStart("0.3.0")
	_, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served))
	if err == nil || !strings.Contains(err.Error(), "failed its self-test") {
		t.Fatalf("got %v", err)
	}
	if r.exeContent() != string(build("0.2.0")) || r.st.Pending() != nil || len(r.leftovers()) != 0 {
		t.Error("nothing may change")
	}
	if _, err := os.Stat(r.up.PreviousPath()); err == nil {
		t.Error("no previous copy should be made for a build that was refused")
	}
}

func TestAnUpdateToTheVersionAlreadyRunningDoesNothing(t *testing.T) {
	r := newRig(t)
	out, err := r.up.Apply(context.Background(), r.params("0.2.0", r.served))
	if err != nil || out.Changed {
		t.Fatalf("%+v %v", out, err)
	}
	if r.exeContent() != string(build("0.2.0")) {
		t.Error("nothing may change")
	}
}

func TestRefusesWithoutAChecksumForThisCPUOrWithAnUnconfirmedUpdateOrABadResponse(t *testing.T) {
	r := newRig(t)
	if _, err := r.up.Apply(context.Background(), Params{Version: "0.3.0", SHA256: map[string]string{"riscv64": sum(r.served)}}); err == nil || !strings.Contains(err.Error(), "no checksum for this machine's CPU") {
		t.Errorf("got %v", err)
	}
	r.status = 404
	if _, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served)); err == nil || !strings.Contains(err.Error(), "answered 404") {
		t.Errorf("got %v", err)
	}
	r.status = 200
	_ = r.st.SetPending(state.PendingUpdate{From: "0.1.0", To: "0.2.0"})
	if _, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served)); err == nil || !strings.Contains(err.Error(), "not been confirmed") {
		t.Errorf("got %v", err)
	}
}

func TestParseParams(t *testing.T) {
	ok, err := ParseParams(map[string]any{"version": "0.3.0", "sha256": map[string]any{"amd64": strings.Repeat("a", 64), "arm64": strings.Repeat("b", 64)}})
	if err != nil || ok.Version != "0.3.0" || len(ok.SHA256) != 2 {
		t.Fatalf("%+v %v", ok, err)
	}
	for _, bad := range []map[string]any{
		{}, {"version": "latest"}, {"version": "0.3.0; rm -rf /"}, {"version": "0.3.0", "sha256": map[string]any{"amd64": "short"}},
		{"version": "0.3.0", "sha256": map[string]any{"amd64": strings.Repeat("A", 64)}}, // upper case is not what the platform sends
	} {
		if _, err := ParseParams(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func guardFor(r *rig, version string, now time.Time) Guard {
	return Guard{ExePath: r.exe, Version: version, State: r.st, Now: func() time.Time { return now }}
}

func TestANewVersionIsConfirmedByItsFirstCheckInAndThenForgotten(t *testing.T) {
	r := newRig(t)
	if _, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served)); err != nil {
		t.Fatal(err)
	}
	g := guardFor(r, "0.3.0", time.Now())
	if g.Started() {
		t.Error("the first start is not a reason to roll back")
	}
	g.Confirm()
	if r.st.Pending() != nil {
		t.Error("a confirmed update must stop being pending")
	}
	reloaded, _ := state.Load(r.stateD)
	if reloaded != nil && reloaded.Pending() != nil {
		t.Error("and that must be on disk")
	}
}

func TestAVersionThatKeepsRestartingWithoutConfirmingIsRolledBack(t *testing.T) {
	r := newRig(t)
	if _, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served)); err != nil {
		t.Fatal(err)
	}
	g := guardFor(r, "0.3.0", time.Now())
	for i := 1; i <= MaxStarts; i++ {
		if g.Started() {
			t.Fatalf("start %d should still be allowed", i)
		}
	}
	if !g.Started() {
		t.Fatal("the start after the limit must ask for a rollback")
	}
	if err := g.Rollback(); err != nil {
		t.Fatal(err)
	}
	if r.exeContent() != string(build("0.2.0")) || r.st.Pending() != nil {
		t.Error("the previous build must be back and nothing pending")
	}
}

func TestAVersionThatCannotCheckInWithinTheTimeIsOverdue(t *testing.T) {
	r := newRig(t)
	if _, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served)); err != nil {
		t.Fatal(err)
	}
	at := r.up.now()
	if guardFor(r, "0.3.0", at).Overdue(at.Add(ConfirmWithin - time.Second)) {
		t.Error("not overdue yet")
	}
	if !guardFor(r, "0.3.0", at).Overdue(at.Add(ConfirmWithin + time.Second)) {
		t.Error("overdue")
	}
	// the old version running again is not judged by the new version's clock
	if guardFor(r, "0.2.0", at).Overdue(at.Add(time.Hour)) {
		t.Error("only the version being confirmed can be overdue")
	}
}

func TestAfterARollbackTheOldVersionForgetsThePendingRecord(t *testing.T) {
	r := newRig(t)
	if _, err := r.up.Apply(context.Background(), r.params("0.3.0", r.served)); err != nil {
		t.Fatal(err)
	}
	if guardFor(r, "0.2.0", time.Now()).Started() {
		t.Error("the old version running again must not be rolled back")
	}
	if r.st.Pending() != nil {
		t.Error("a pending record for a version that is not running is stale")
	}
}

func TestARollbackWithoutAPreviousBuildSaysSoAndStillClearsPending(t *testing.T) {
	r := newRig(t)
	_ = r.st.SetPending(state.PendingUpdate{From: "0.2.0", To: "0.3.0", At: time.Now(), Previous: filepath.Join(r.dir, "gone")})
	if err := guardFor(r, "0.3.0", time.Now()).Rollback(); err == nil {
		t.Error("a missing previous build is an error")
	}
	if r.st.Pending() != nil {
		t.Error("but it must not loop forever")
	}
}
