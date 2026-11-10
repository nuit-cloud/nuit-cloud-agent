package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

var (
	oldKey  = taskverify.Key{KeyID: "fe812c12f3ab4ce6", PublicKey: "6kpsY-KcUgq-9VB7Ey7F-ZVHdq6-vnuSQh7qaRRG0iw"}
	newKey  = taskverify.Key{KeyID: "0123456789abcdef", PublicKey: "new-public-key"}
	nextKey = taskverify.Key{KeyID: "aaaaaaaaaaaaaaaa", PublicKey: "next-public-key"}
)

// An agent upgraded in place reads the state file its previous version wrote (one key, trusted at enrollment) and keeps trusting that key.
func TestAStateFileFromAnEarlierAgentKeepsTheKeyItTrusted(t *testing.T) {
	dir := t.TempDir()
	written := `{
  "platformUrl": "https://platform.example",
  "machineId": "m-1",
  "name": "box",
  "credential": "nca_secret",
  "trustedKeys": [{"keyId": "fe812c12f3ab4ce6", "publicKey": "6kpsY-KcUgq-9VB7Ey7F-ZVHdq6-vnuSQh7qaRRG0iw"}],
  "enrolledAt": "2026-10-20T12:00:00Z",
  "seen": {}
}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if keys := s.Keys(); len(keys) != 1 || keys[0] != oldKey {
		t.Fatalf("keys: %+v", keys)
	}
	// and it can take a second key from there, which survives the next restart
	if changed, err := s.TrustKey(newKey); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	reloaded, _ := Load(dir)
	if ids := reloaded.KeyIDs(); len(ids) != 2 || ids[0] != oldKey.KeyID || ids[1] != newKey.KeyID {
		t.Errorf("after a restart: %v", ids)
	}
}

func TestTrustingAKeyIsSavedAtOnceAndTrustingItAgainChangesNothing(t *testing.T) {
	dir := t.TempDir()
	s := enrolled(dir)
	s.TrustedKeys = []taskverify.Key{oldKey}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.TrustKey(newKey); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	if changed, err := s.TrustKey(newKey); err != nil || changed {
		t.Fatalf("a key already trusted: %v %v", changed, err)
	}
	if _, err := s.TrustKey(taskverify.Key{KeyID: newKey.KeyID, PublicKey: "a-different-key"}); !errors.Is(err, ErrKeyIDClash) {
		t.Errorf("another key under a trusted id: %v", err)
	}
	reloaded, _ := Load(dir)
	if len(reloaded.Keys()) != 2 {
		t.Errorf("not on disk: %+v", reloaded.Keys())
	}
	info, _ := os.Stat(filepath.Join(dir, "state.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v, want 0600", info.Mode().Perm())
	}
}

func TestTheNumberOfTrustedKeysIsBounded(t *testing.T) {
	s := enrolled(t.TempDir())
	s.TrustedKeys = nil
	for i := 0; i < MaxTrustedKeys; i++ {
		s.TrustedKeys = append(s.TrustedKeys, taskverify.Key{KeyID: strings.Repeat(string(rune('a'+i)), 16), PublicKey: "p"})
	}
	if _, err := s.TrustKey(newKey); !errors.Is(err, ErrTooManyKeys) {
		t.Errorf("got %v", err)
	}
}

// A retire can never lock the agent out: not of the key that signed it, not of the last key.
func TestRetiringAKeyNeverRemovesTheSigningKeyOrTheLastOne(t *testing.T) {
	dir := t.TempDir()
	s := enrolled(dir)
	s.TrustedKeys = []taskverify.Key{oldKey, newKey}
	if _, err := s.RetireKey(oldKey.KeyID, oldKey.KeyID); !errors.Is(err, ErrRetireSigner) {
		t.Errorf("retiring the signing key: %v", err)
	}
	if len(s.Keys()) != 2 {
		t.Fatalf("a refused retire changed the keys: %+v", s.Keys())
	}
	if changed, err := s.RetireKey(nextKey.KeyID, newKey.KeyID); err != nil || changed {
		t.Errorf("a key not trusted: %v %v", changed, err)
	}
	if changed, err := s.RetireKey(oldKey.KeyID, newKey.KeyID); err != nil || !changed {
		t.Fatalf("retiring the old key with a task signed by the new: %v %v", changed, err)
	}
	reloaded, _ := Load(dir)
	if ids := reloaded.KeyIDs(); len(ids) != 1 || ids[0] != newKey.KeyID {
		t.Errorf("on disk: %v", ids)
	}
	if _, err := s.RetireKey(newKey.KeyID, oldKey.KeyID); !errors.Is(err, ErrRetireLastKey) {
		t.Errorf("retiring the last key: %v", err)
	}
	if ids := s.KeyIDs(); len(ids) != 1 {
		t.Errorf("the last key was removed: %v", ids)
	}
}
