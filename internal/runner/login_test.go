package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/profiles"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

// sshKey returns the base64 body of a fresh ssh-ed25519 public key.
func sshKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := binary.BigEndian.AppendUint32(nil, 11)
	blob = append(blob, "ssh-ed25519"...)
	blob = binary.BigEndian.AppendUint32(blob, 32)
	blob = append(blob, pub...)
	return base64.StdEncoding.EncodeToString(blob)
}

func loginTask(login any) *taskverify.Task {
	return &taskverify.Task{Name: "profile-apply", TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-cloud-login", "login": login}}
}

func registry(keys ...map[string]any) map[string]any {
	list := make([]any, len(keys))
	for i, k := range keys {
		list[i] = k
	}
	return map[string]any{"format": float64(2), "keys": list}
}

func TestTheKeyRegistryReachesTheScriptAsIdsAndBodiesOnly(t *testing.T) {
	a, b := sshKey(t), sshKey(t)
	f := &fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_login", Status: "applied"}}}}
	r := withProfiles(f)
	rep := r.Run(context.Background(), loginTask(registry(
		map[string]any{"id": "platform", "publicKey": "ssh-ed25519 " + a + " nuit-cloud platform login key"},
		map[string]any{"id": "mj-laptop-2", "publicKey": "ssh-ed25519 " + b},
	)))
	want := "NCS_LOGIN_KEYS=platform=" + a + " mj-laptop-2=" + b
	if rep.Status != "done" || len(f.env) != 1 || f.env[0] != want {
		t.Errorf("%+v env=%v", rep, f.env)
	}
	if strings.Contains(strings.Join(f.env, " "), "login key") {
		t.Error("a key's comment reached the script")
	}
}

func TestTheOldSingleKeyFormStillWorksAsBefore(t *testing.T) {
	a := sshKey(t)
	f := &fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_login", Status: "applied"}}}}
	r := withProfiles(f)
	rep := r.Run(context.Background(), loginTask(map[string]any{"publicKey": "ssh-ed25519 " + a + " nuit-cloud-platform"}))
	if rep.Status != "done" || len(f.env) != 1 || f.env[0] != "NCS_PLATFORM_SSH_KEY=ssh-ed25519 "+a+" nuit-cloud-platform" {
		t.Errorf("%+v env=%v", rep, f.env)
	}
	f.env = nil
	if rep := r.Run(context.Background(), loginTask(map[string]any{"publicKey": "ssh-rsa AAAA x"})); rep.Status != "failed" || f.env != nil {
		t.Errorf("an rsa key was accepted in the old form: %+v", rep)
	}
}

func TestAKeyRegistryThatIsNotExactlyRightChangesNothing(t *testing.T) {
	a, b := sshKey(t), sshKey(t)
	good := func(id, body string) map[string]any {
		return map[string]any{"id": id, "publicKey": "ssh-ed25519 " + body}
	}
	tooMany := make([]map[string]any, 65)
	for i := range tooMany {
		tooMany[i] = good("k"+strings.Repeat("x", i%30)+string(rune('a'+i%26))+string(rune('a'+i/26)), sshKey(t))
	}
	notEd25519 := base64.StdEncoding.EncodeToString(append(binary.BigEndian.AppendUint32(nil, 11), []byte("ssh-ed25518"+strings.Repeat("\x00", 36))...))
	cases := map[string]any{
		"an empty list":            registry(),
		"no keys at all":           map[string]any{"format": float64(2)},
		"keys not a list":          map[string]any{"format": float64(2), "keys": "platform"},
		"another format":           map[string]any{"format": float64(3), "keys": []any{good("platform", a)}},
		"format as text":           map[string]any{"format": "2", "keys": []any{good("platform", a)}},
		"no format":                map[string]any{"keys": []any{good("platform", a)}},
		"an extra field":           map[string]any{"format": float64(2), "keys": []any{good("platform", a)}, "publicKey": "ssh-ed25519 " + a},
		"65 keys":                  registry(tooMany...),
		"duplicate id":             registry(good("platform", a), good("platform", b)),
		"duplicate body":           registry(good("platform", a), good("laptop", a)),
		"upper-case id":            registry(good("Platform", a)),
		"id starting with -":       registry(good("-platform", a)),
		"id of 41":                 registry(good(strings.Repeat("a", 41), a)),
		"shell in the id":          registry(good("a$(reboot)", a)),
		"space in the id":          registry(good("a b", a)),
		"empty id":                 registry(good("", a)),
		"rsa key":                  registry(map[string]any{"id": "platform", "publicKey": "ssh-rsa " + a}),
		"options before the key":   registry(map[string]any{"id": "platform", "publicKey": `command="reboot" ssh-ed25519 ` + a}),
		"a second line":            registry(map[string]any{"id": "platform", "publicKey": "ssh-ed25519 " + a + "\nssh-ed25519 " + b}),
		"newline in the comment":   registry(map[string]any{"id": "platform", "publicKey": "ssh-ed25519 " + a + " x\n"}),
		"short body":               registry(good("platform", a[:60])),
		"body not ed25519 inside":  registry(good("platform", notEd25519)),
		"not an object":            registry(map[string]any{}),
		"extra field on a key":     registry(map[string]any{"id": "platform", "publicKey": "ssh-ed25519 " + a, "purpose": "x"}),
		"login not an object":      "ssh-ed25519 " + a,
		"key entry not an object":  map[string]any{"format": float64(2), "keys": []any{"ssh-ed25519 " + a}},
		"old form with a bad key":  map[string]any{"publicKey": "ssh-ed25519 " + a + "; reboot"},
		"old form with a new line": map[string]any{"publicKey": "ssh-ed25519 " + a + "\nssh-ed25519 " + b},
	}
	for name, login := range cases {
		f := &fakeProfiles{}
		rep := withProfiles(f).Run(context.Background(), loginTask(login))
		if rep.Status != "failed" || len(f.calls) != 0 {
			t.Errorf("%s was accepted: %+v calls=%v", name, rep, f.calls)
		}
	}
	// the check refuses too: nothing reaches the scripts
	f := &fakeProfiles{}
	task := loginTask(registry())
	task.Name = "profile-check"
	if rep := withProfiles(f).Run(context.Background(), task); rep.Status != "failed" || len(f.calls) != 0 || !strings.Contains(rep.Output, "empty") {
		t.Errorf("an empty list reached a check: %+v", rep)
	}
}

func TestSixtyFourKeysAreAccepted(t *testing.T) {
	keys := make([]map[string]any, 64)
	for i := range keys {
		keys[i] = map[string]any{"id": "k" + string(rune('a'+i%26)) + string(rune('a'+i/26)), "publicKey": "ssh-ed25519 " + sshKey(t)}
	}
	f := &fakeProfiles{}
	if rep := withProfiles(f).Run(context.Background(), loginTask(registry(keys...))); rep.Status != "done" || len(f.env) != 1 || len(strings.Fields(f.env[0])) != 64 {
		t.Errorf("%+v", rep)
	}
}
