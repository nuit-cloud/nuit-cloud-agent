package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

func enrolled(dir string) *State {
	s := New(dir)
	s.PlatformURL = "https://platform.example"
	s.MachineID = "m-1"
	s.Name = "box"
	s.Credential = "nca_secret"
	s.TrustedKeys = []taskverify.Key{{KeyID: "k", PublicKey: "p"}}
	s.EnrolledAt = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return s
}

func TestSavedStateComesBackAndIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if err := enrolled(dir).Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineID != "m-1" || got.Credential != "nca_secret" || len(got.TrustedKeys) != 1 {
		t.Errorf("round trip lost something: %+v", got)
	}
	info, _ := os.Stat(filepath.Join(dir, "state.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v, want 0600", info.Mode().Perm())
	}
	dirInfo, _ := os.Stat(dir)
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode %v, want 0700", dirInfo.Mode().Perm())
	}
}

func TestNothingLeftBehindByASave(t *testing.T) {
	dir := t.TempDir()
	s := enrolled(dir)
	for i := 0; i < 3; i++ {
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Errorf("unexpected files: %v", entries)
	}
}

func TestNotEnrolledAndDamagedAreSaidApart(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600)
	if _, err := Load(dir); err == nil || errors.Is(err, ErrNotEnrolled) {
		t.Errorf("a damaged file must be an error of its own, got %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"platformUrl":"x"}`), 0o600)
	if _, err := Load(dir); err == nil {
		t.Error("an incomplete state must be refused")
	}
}

func TestATaskIsRememberedOnDiskBeforeItRunsAndForgottenAfterTwoDays(t *testing.T) {
	dir := t.TempDir()
	s := enrolled(dir)
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := s.MarkSeen("old", t0); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := Load(dir)
	if !reloaded.HasSeen("old") {
		t.Fatal("a task marked seen must survive a restart")
	}
	if err := s.MarkSeen("new", t0.Add(SeenFor+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s.HasSeen("old") || !s.HasSeen("new") {
		t.Errorf("old ids should be pruned, new kept: %+v", s.Seen)
	}
}

func TestACredentialChangeIsSavedAtOnce(t *testing.T) {
	dir := t.TempDir()
	s := enrolled(dir)
	if err := s.SetCredential("nca_new"); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := Load(dir)
	if reloaded.Credential != "nca_new" {
		t.Errorf("got %q", reloaded.Credential)
	}
}
