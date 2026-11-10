package loop

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/runner"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

// The vectors' platform key: their note says the public test seed is 32 bytes of 0x07.
func platformKey(t *testing.T, v vectors) ed25519.PrivateKey {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	if base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)) != v.TrustedKeys[0].PublicKey {
		t.Fatal("the vectors' key is not the 0x07 seed")
	}
	return priv
}

func signed(t *testing.T, priv ed25519.PrivateKey, keyID, machineID, id, name string, params map[string]any) taskverify.Envelope {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"v": 1, "id": id, "machineId": machineID, "name": name, "params": params,
		"issuedAt": "2026-10-20T12:00:00.000Z", "expiresAt": "2026-10-20T12:10:00.000Z", "timeoutSeconds": 30})
	if err != nil {
		t.Fatal(err)
	}
	return taskverify.Envelope{KeyID: keyID, Payload: base64.RawURLEncoding.EncodeToString(payload),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(taskverify.SigningDomain), payload...)))}
}

func generated(t *testing.T) (taskverify.Key, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return taskverify.Key{KeyID: taskverify.KeyIDOf(pub), PublicKey: base64.RawURLEncoding.EncodeToString(pub)}, priv
}

func once(t *testing.T, l *Loop, p *fakePlatform, env taskverify.Envelope) {
	t.Helper()
	p.mu.Lock()
	p.offer = []taskverify.Envelope{env}
	p.mu.Unlock()
	if _, err := l.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Wait()
}

// The ticket's acceptance (nuit-cloud/nuit-cloud#64): the agent adds a key only from a task signed by a key it already trusts. A
// trust-key signed by any other key, under its own id or a trusted one's, is refused and changes nothing.
func TestAKeyIsTrustedOnlyFromATaskSignedByATrustedKey(t *testing.T) {
	l, p, _, v := setup(t)
	l.Runner = runner.Runner{Keys: l.State}
	attacker, attackerPriv := generated(t)
	params := map[string]any{"keyId": attacker.KeyID, "publicKey": attacker.PublicKey}

	once(t, l, p, signed(t, attackerPriv, attacker.KeyID, v.MachineID, "cccccccc-0000-0000-0000-000000000001", "trust-key", params))
	once(t, l, p, signed(t, attackerPriv, v.TrustedKeys[0].KeyID, v.MachineID, "cccccccc-0000-0000-0000-000000000002", "trust-key", params))
	if ids := l.State.KeyIDs(); len(ids) != 1 || ids[0] != v.TrustedKeys[0].KeyID {
		t.Fatalf("an untrusted signer changed the keys: %v", ids)
	}
	if len(p.reports) != 0 {
		t.Errorf("an unauthentic task was reported on: %+v", p.reports)
	}

	// The rollover: the platform's current key signs a trust-key carrying the next key; then the next key's tasks run, and it retires the old.
	next, nextPriv := generated(t)
	once(t, l, p, signed(t, platformKey(t, v), v.TrustedKeys[0].KeyID, v.MachineID, "cccccccc-0000-0000-0000-000000000003", "trust-key",
		map[string]any{"keyId": next.KeyID, "publicKey": next.PublicKey}))
	if got := p.statuses("cccccccc-0000-0000-0000-000000000003"); len(got) != 2 || got[1] != "done" {
		t.Fatalf("trust-key signed by the trusted key: %v", got)
	}
	once(t, l, p, signed(t, nextPriv, next.KeyID, v.MachineID, "cccccccc-0000-0000-0000-000000000004", "retire-key",
		map[string]any{"keyId": v.TrustedKeys[0].KeyID}))
	if got := p.statuses("cccccccc-0000-0000-0000-000000000004"); len(got) != 2 || got[1] != "done" {
		t.Fatalf("retire-key signed by the new key: %v", got)
	}
	if ids := l.State.KeyIDs(); len(ids) != 1 || ids[0] != next.KeyID {
		t.Fatalf("after the rollover: %v", ids)
	}
	// the retired key is no longer trusted for anything
	once(t, l, p, v.Tasks["ping"])
	if got := p.statuses(pingID); len(got) != 0 {
		t.Errorf("a task signed by the retired key ran: %v", got)
	}
}
