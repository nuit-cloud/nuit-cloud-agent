package taskverify

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

type vectors struct {
	MachineID   string              `json:"machineId"`
	TrustedKeys []Key               `json:"trustedKeys"`
	Tasks       map[string]Envelope `json:"tasks"`
}

func load(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

var (
	inWindow = time.Date(2026, 10, 20, 12, 5, 0, 0, time.UTC)
	catalog  = []string{"ping", "survey", "profile-check", "profile-apply"}
)

func ctx(v vectors) Context {
	return Context{MachineID: v.MachineID, Now: inWindow, KnownTasks: catalog}
}

// What the TypeScript platform signs, this agent verifies: the whole point of the test vectors.
func TestAcceptsWhatThePlatformSigns(t *testing.T) {
	v := load(t)
	for name, env := range v.Tasks {
		task, err := Verify(env, v.TrustedKeys, ctx(v))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if task.MachineID != v.MachineID || task.V != 1 || task.TimeoutSeconds == 0 {
			t.Errorf("%s: unexpected task %+v", name, task)
		}
	}
	task, _ := Verify(v.Tasks["profileApply"], v.TrustedKeys, ctx(v))
	if task.Name != "profile-apply" || task.Params["profile"] != "nuit-cloud-server" {
		t.Errorf("parameters not read from the signed bytes: %+v", task)
	}
}

// Characters Go's encoder would escape (<, >, &) and non-ASCII must not matter: the signed bytes are verified as sent.
func TestVerifiesTheBytesAsSentNotARebuiltDocument(t *testing.T) {
	v := load(t)
	task, err := Verify(v.Tasks["tricky"], v.TrustedKeys, ctx(v))
	if err != nil {
		t.Fatal(err)
	}
	if task.Params["note"] != "<b>&amp; café ✓</b>" {
		t.Errorf("got %q", task.Params["note"])
	}
}

func refusedWith(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want refusal %q", want)
	}
	if !IsRefusal(err) || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestRefusesAForgedSignature(t *testing.T) {
	v := load(t)
	env := v.Tasks["ping"]
	sig := []byte(env.Signature)
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	env.Signature = string(sig)
	_, err := Verify(env, v.TrustedKeys, ctx(v))
	refusedWith(t, err, "the signature does not verify")
}

func TestRefusesAnAlteredPayload(t *testing.T) {
	v := load(t)
	env := v.Tasks["profileApply"]
	payload, _ := base64.RawURLEncoding.DecodeString(env.Payload)
	var doc map[string]any
	_ = json.Unmarshal(payload, &doc)
	doc["params"] = map[string]any{"profile": "nuit-container-service"}
	altered, _ := json.Marshal(doc)
	env.Payload = base64.RawURLEncoding.EncodeToString(altered)
	_, err := Verify(env, v.TrustedKeys, ctx(v))
	refusedWith(t, err, "the signature does not verify")
}

func TestRefusesAKeyItDoesNotTrust(t *testing.T) {
	v := load(t)
	_, err := Verify(v.Tasks["ping"], nil, ctx(v))
	refusedWith(t, err, "signed by a key this machine does not trust")

	// signed properly by an attacker's own key, presented under the trusted key id
	pub, priv, _ := ed25519.GenerateKey(nil)
	_ = pub
	env := v.Tasks["ping"]
	payload, _ := base64.RawURLEncoding.DecodeString(env.Payload)
	env.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(SigningDomain), payload...)))
	_, err = Verify(env, v.TrustedKeys, ctx(v))
	refusedWith(t, err, "the signature does not verify")
}

func TestRefusesATaskForAnotherMachine(t *testing.T) {
	v := load(t)
	c := ctx(v)
	c.MachineID = "99999999-9999-9999-9999-999999999999"
	_, err := Verify(v.Tasks["ping"], v.TrustedKeys, c)
	refusedWith(t, err, "the task is for another machine")
}

func TestRefusesAnExpiredTask(t *testing.T) {
	v := load(t)
	c := ctx(v)
	c.Now = time.Date(2026, 10, 20, 12, 10, 0, 0, time.UTC) // exactly at expiry: expired
	_, err := Verify(v.Tasks["ping"], v.TrustedKeys, c)
	refusedWith(t, err, "the task has expired")
}

// A task the platform really signed but this agent must not run can be reported on; anything unauthentic cannot.
func TestARefusalNamesTheTaskOnlyWhenItIsAuthentic(t *testing.T) {
	v := load(t)
	c := ctx(v)
	c.KnownTasks = []string{"ping"}
	_, err := Verify(v.Tasks["profileApply"], v.TrustedKeys, c)
	var r *Refusal
	if !errors.As(err, &r) || r.Task == nil || r.Task.ID != "aaaaaaaa-0000-0000-0000-000000000003" {
		t.Fatalf("authentic refusal should carry the task: %+v", err)
	}
	env := v.Tasks["ping"]
	env.Signature = v.Tasks["survey"].Signature
	_, err = Verify(env, v.TrustedKeys, ctx(v))
	if !errors.As(err, &r) || r.Task != nil {
		t.Fatalf("an unauthentic task must not be named: %+v", err)
	}
	c = ctx(v)
	c.MachineID = "99999999-9999-9999-9999-999999999999"
	_, err = Verify(v.Tasks["ping"], v.TrustedKeys, c)
	if !errors.As(err, &r) || r.Task != nil {
		t.Fatalf("another machine's task is not ours to report on: %+v", err)
	}
}

func TestRefusesAReplay(t *testing.T) {
	v := load(t)
	c := ctx(v)
	c.AlreadySeen = func(id string) bool { return id == "aaaaaaaa-0000-0000-0000-000000000001" }
	_, err := Verify(v.Tasks["ping"], v.TrustedKeys, c)
	refusedWith(t, err, "the task was already run")
}

func TestRefusesATaskOutsideItsOwnCatalog(t *testing.T) {
	v := load(t)
	c := ctx(v)
	c.KnownTasks = []string{"ping"}
	_, err := Verify(v.Tasks["profileApply"], v.TrustedKeys, c)
	refusedWith(t, err, "the task is not in this agent's catalog")
}

func TestRefusesGarbageWithoutPanicking(t *testing.T) {
	v := load(t)
	for _, env := range []Envelope{
		{},
		{KeyID: v.TrustedKeys[0].KeyID},
		{KeyID: v.TrustedKeys[0].KeyID, Payload: "!!!", Signature: "!!!"},
		{KeyID: v.TrustedKeys[0].KeyID, Payload: "e30", Signature: "AAAA"},
	} {
		if _, err := Verify(env, v.TrustedKeys, ctx(v)); err == nil || !IsRefusal(err) {
			t.Errorf("%+v: got %v", env, err)
		}
	}
	bad := []Key{{KeyID: v.TrustedKeys[0].KeyID, PublicKey: "short"}}
	_, err := Verify(v.Tasks["ping"], bad, ctx(v))
	refusedWith(t, err, "the trusted key is malformed")
}

// signWith signs a task the way the platform does, with a key of the test's own.
func signWith(t *testing.T, priv ed25519.PrivateKey, keyID string, v vectors, name string) Envelope {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"v": 1, "id": "bbbbbbbb-0000-0000-0000-00000000000" + name[:1], "machineId": v.MachineID, "name": name, "params": map[string]any{},
		"issuedAt": "2026-10-20T12:00:00.000Z", "expiresAt": "2026-10-20T12:10:00.000Z", "timeoutSeconds": 30})
	if err != nil {
		t.Fatal(err)
	}
	return Envelope{KeyID: keyID, Payload: base64.RawURLEncoding.EncodeToString(payload),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(SigningDomain), payload...)))}
}

func newKey(t *testing.T) (Key, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return Key{KeyID: KeyIDOf(pub), PublicKey: base64.RawURLEncoding.EncodeToString(pub)}, priv
}

// During a rollover the agent trusts two keys and a task signed by either runs; the verified task says which key signed it.
func TestDuringARolloverATaskSignedByEitherTrustedKeyIsAccepted(t *testing.T) {
	v := load(t)
	next, priv := newKey(t)
	both := append(append([]Key(nil), v.TrustedKeys...), next)

	old, err := Verify(v.Tasks["ping"], both, ctx(v))
	if err != nil || old.SignedBy != v.TrustedKeys[0].KeyID {
		t.Fatalf("signed by the old key: %+v %v", old, err)
	}
	fresh, err := Verify(signWith(t, priv, next.KeyID, v, "survey"), both, ctx(v))
	if err != nil || fresh.SignedBy != next.KeyID {
		t.Fatalf("signed by the new key: %+v %v", fresh, err)
	}
	// once the old key is retired, only the new one verifies
	_, err = Verify(v.Tasks["ping"], []Key{next}, ctx(v))
	refusedWith(t, err, "signed by a key this machine does not trust")
}

// Only the key the task names is tried: a signature by one trusted key under the other's id, or under no id, is refused.
func TestOnlyTheKeyTheTaskNamesIsTried(t *testing.T) {
	v := load(t)
	next, priv := newKey(t)
	both := append(append([]Key(nil), v.TrustedKeys...), next)
	env := signWith(t, priv, v.TrustedKeys[0].KeyID, v, "survey")
	_, err := Verify(env, both, ctx(v))
	refusedWith(t, err, "the signature does not verify")
	env.KeyID = ""
	_, err = Verify(env, append(both, Key{KeyID: "", PublicKey: next.PublicKey}), ctx(v))
	refusedWith(t, err, "signed by a key this machine does not trust")
}

// The agent names a key exactly as the platform does (keyIdOf in task-signing.ts): the vectors' key id is derived from its key.
func TestAKeyIdIsDerivedAsThePlatformDerivesIt(t *testing.T) {
	v := load(t)
	raw, _ := base64.RawURLEncoding.DecodeString(v.TrustedKeys[0].PublicKey)
	if got := KeyIDOf(raw); got != v.TrustedKeys[0].KeyID {
		t.Errorf("got %s, the platform says %s", got, v.TrustedKeys[0].KeyID)
	}
}

func TestATrustKeyTaskCarriesOneWellFormedKeyUnderItsOwnId(t *testing.T) {
	v := load(t)
	good := v.TrustedKeys[0]
	k, err := KeyFromParams(map[string]any{"keyId": good.KeyID, "publicKey": good.PublicKey})
	if err != nil || k != good {
		t.Fatalf("%+v %v", k, err)
	}
	other, _ := newKey(t)
	for name, params := range map[string]map[string]any{
		"another key's id":  {"keyId": other.KeyID, "publicKey": good.PublicKey},
		"no id":             {"publicKey": good.PublicKey},
		"upper-case id":     {"keyId": "FE812C12F3AB4CE6", "publicKey": good.PublicKey},
		"id with a newline": {"keyId": good.KeyID + "\n", "publicKey": good.PublicKey},
		"short key":         {"keyId": good.KeyID, "publicKey": good.PublicKey[:40]},
		"padded key":        {"keyId": good.KeyID, "publicKey": good.PublicKey + "="},
		"standard base64":   {"keyId": good.KeyID, "publicKey": "+" + good.PublicKey[1:]},
		"key as a number":   {"keyId": good.KeyID, "publicKey": 42},
		"shell in the key":  {"keyId": good.KeyID, "publicKey": "$(reboot)" + good.PublicKey[9:]},
		"a private key too": {"keyId": good.KeyID, "publicKey": good.PublicKey, "seed": "AAAA"},
		"nothing":           {},
	} {
		if _, err := KeyFromParams(params); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
