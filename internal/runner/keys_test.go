package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/state"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

var enrolledKey = taskverify.Key{KeyID: "fe812c12f3ab4ce6", PublicKey: "6kpsY-KcUgq-9VB7Ey7F-ZVHdq6-vnuSQh7qaRRG0iw"}

func keyRunner(t *testing.T) (Runner, *state.State) {
	t.Helper()
	st := state.New(t.TempDir())
	st.PlatformURL, st.MachineID, st.Credential, st.TrustedKeys = "https://p", "m-1", "nca_x", []taskverify.Key{enrolledKey}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	r := newRunner()
	r.Keys = st
	return r, st
}

func freshKey(t *testing.T) taskverify.Key {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return taskverify.Key{KeyID: taskverify.KeyIDOf(pub), PublicKey: base64.RawURLEncoding.EncodeToString(pub)}
}

func keyTask(name, signedBy string, params map[string]any) *taskverify.Task {
	return &taskverify.Task{Name: name, TimeoutSeconds: 30, Params: params, SignedBy: signedBy}
}

func TestTrustKeyAddsTheKeyAndSaysWhichKeysAreTrusted(t *testing.T) {
	r, st := keyRunner(t)
	next := freshKey(t)
	rep := r.Run(context.Background(), keyTask("trust-key", enrolledKey.KeyID, map[string]any{"keyId": next.KeyID, "publicKey": next.PublicKey}))
	if rep.Status != "done" || *rep.ExitCode != 0 {
		t.Fatalf("%+v", rep)
	}
	res := rep.Result.(map[string]any)
	if ids := res["trustedKeyIds"].([]string); len(ids) != 2 || ids[1] != next.KeyID || res["changed"] != true {
		t.Errorf("%+v", res)
	}
	if keys := st.Keys(); len(keys) != 2 || keys[1] != next {
		t.Errorf("not trusted: %+v", keys)
	}
	again := r.Run(context.Background(), keyTask("trust-key", enrolledKey.KeyID, map[string]any{"keyId": next.KeyID, "publicKey": next.PublicKey}))
	if again.Status != "done" || again.Result.(map[string]any)["changed"] != false || len(st.Keys()) != 2 {
		t.Errorf("trusting a key twice: %+v", again)
	}
}

// A hostile or malformed key changes nothing.
func TestTrustKeyWithABadKeyChangesNothing(t *testing.T) {
	r, st := keyRunner(t)
	next, other := freshKey(t), freshKey(t)
	for name, params := range map[string]map[string]any{
		"another key's id":   {"keyId": other.KeyID, "publicKey": next.PublicKey},
		"shell in the id":    {"keyId": "$(reboot)1234567", "publicKey": next.PublicKey},
		"newline in the key": {"keyId": next.KeyID, "publicKey": next.PublicKey[:42] + "\n"},
		"extra parameter":    {"keyId": next.KeyID, "publicKey": next.PublicKey, "seed": "x"},
		"nothing":            {},
	} {
		rep := r.Run(context.Background(), keyTask("trust-key", enrolledKey.KeyID, params))
		if rep.Status != "failed" {
			t.Errorf("%s: %+v", name, rep)
		}
	}
	if ids := st.KeyIDs(); len(ids) != 1 || ids[0] != enrolledKey.KeyID {
		t.Errorf("the keys changed: %v", ids)
	}
}

// A task that did not come through verification (no signing key recorded) cannot change the keys.
func TestKeyTasksNeedAVerifiedSigner(t *testing.T) {
	r, st := keyRunner(t)
	next := freshKey(t)
	if rep := r.Run(context.Background(), keyTask("trust-key", "", map[string]any{"keyId": next.KeyID, "publicKey": next.PublicKey})); rep.Status != "failed" {
		t.Errorf("%+v", rep)
	}
	if len(st.Keys()) != 1 {
		t.Errorf("the keys changed: %+v", st.Keys())
	}
}

func TestRetireKeyRefusesTheSigningKeyAndTheLastKeyAndSaysWhy(t *testing.T) {
	r, st := keyRunner(t)
	next := freshKey(t)
	if _, err := st.TrustKey(next); err != nil {
		t.Fatal(err)
	}
	rep := r.Run(context.Background(), keyTask("retire-key", enrolledKey.KeyID, map[string]any{"keyId": enrolledKey.KeyID}))
	if rep.Status != "failed" || !strings.Contains(rep.Output, "signed by the key it would retire") {
		t.Errorf("retiring the signing key: %+v", rep)
	}
	rep = r.Run(context.Background(), keyTask("retire-key", next.KeyID, map[string]any{"keyId": enrolledKey.KeyID}))
	if rep.Status != "done" || rep.Result.(map[string]any)["changed"] != true {
		t.Fatalf("retiring the old key with a task signed by the new: %+v", rep)
	}
	if ids := st.KeyIDs(); len(ids) != 1 || ids[0] != next.KeyID {
		t.Fatalf("keys: %v", ids)
	}
	// the one key left cannot be retired, whoever signed it
	rep = r.Run(context.Background(), keyTask("retire-key", enrolledKey.KeyID, map[string]any{"keyId": next.KeyID}))
	if rep.Status != "failed" || !strings.Contains(rep.Output, "only key") {
		t.Errorf("retiring the last key: %+v", rep)
	}
	// a key no longer trusted: nothing to do
	rep = r.Run(context.Background(), keyTask("retire-key", next.KeyID, map[string]any{"keyId": enrolledKey.KeyID}))
	if rep.Status != "done" || rep.Result.(map[string]any)["changed"] != false {
		t.Errorf("retiring a key not trusted: %+v", rep)
	}
	for _, bad := range []map[string]any{{"keyId": "FE812C12F3AB4CE6"}, {"keyId": next.KeyID, "also": "x"}, {}} {
		if rep := r.Run(context.Background(), keyTask("retire-key", next.KeyID, bad)); rep.Status != "failed" {
			t.Errorf("%v: %+v", bad, rep)
		}
	}
	if len(st.Keys()) != 1 {
		t.Errorf("the keys changed: %+v", st.Keys())
	}
}

func TestPingSaysWhichKeysAreTrustedAndTheCatalogHasTheKeyTasks(t *testing.T) {
	r, _ := keyRunner(t)
	rep := r.Run(context.Background(), &taskverify.Task{Name: "ping", TimeoutSeconds: 30})
	if ids, _ := rep.Result.(map[string]any)["trustedKeyIds"].([]string); len(ids) != 1 || ids[0] != enrolledKey.KeyID {
		t.Errorf("%+v", rep.Result)
	}
	found := map[string]bool{}
	for _, n := range r.Names() {
		found[n] = true
	}
	if !found["trust-key"] || !found["retire-key"] {
		t.Errorf("catalog: %v", r.Names())
	}
}
