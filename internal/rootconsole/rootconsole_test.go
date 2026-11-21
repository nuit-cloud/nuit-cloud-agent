package rootconsole

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Real hashes (of throwaway words): openssl passwd -6, and Debian trixie's mkpasswd -m yescrypt.
const (
	sha512Hash   = "$6$Xw3nQm1pZr8tLs0a$lDwWrLKgJ072NNOWKl9n2RvpvOFl8zIGSPVAwF3AReFCzfxb.JQ8uGCDjZpOiKJ4Bu62Dij1HRReSDKFnXMDE0"
	sha512Slash  = "$6$F5G1hZpT/L7RswVe$uo8Wu638JjE.hXwLupVhqBmRdCFgvktU/RS4qa6O8XhNB89IXEbklGwDIL2/Eqq1PMqGOix82XY4EiKqDYznv."
	yescryptHash = "$y$j9T$uZLQoZM5bHxvkYTzn31GU1$sbYUCQiGjE2Z4m9jioAFrkmM3vytg9onLM81u8t4xo4"
	yescryptR7   = "$y$jBT$UJwhRyCoS9./E/wjPBsGO.$esAG9nmVBAETNvTX4UOy7AHgTBOXkj.5c32/g7wxdb0"
)

var sha512Rounds = "$6$rounds=656000$Xw3nQm1pZr8tLs0a$" + strings.Repeat("a", 86)

func TestGoodHashesAreAccepted(t *testing.T) {
	for hash, scheme := range map[string]string{sha512Hash: "sha512-crypt", sha512Slash: "sha512-crypt", sha512Rounds: "sha512-crypt",
		yescryptHash: "yescrypt", yescryptR7: "yescrypt"} {
		p, err := Parse(map[string]any{"action": "set", "hash": hash})
		if err != nil || p.Action != "set" || p.Hash != hash || p.Scheme != scheme {
			t.Errorf("%s: %+v %v", scheme, p, err)
		}
	}
	if p, err := Parse(map[string]any{"action": "lock"}); err != nil || p.Action != "lock" || p.Hash != "" {
		t.Errorf("lock: %+v %v", p, err)
	}
}

func TestEveryMalformedTaskIsRefusedWithoutNamingTheHash(t *testing.T) {
	bad := []map[string]any{
		{},
		{"action": "set"},
		{"action": "set", "hash": ""},
		{"action": "set", "hash": nil},
		{"action": "set", "hash": 6},
		{"action": "set", "hash": "hunter2-plaintext"},
		{"action": "set", "hash": "$1$abc$OGyl6dDvZCDiGmIVbeuCq/"},
		{"action": "set", "hash": "$5$abc$sCOGUciM.GdmYMCUvmUcgu03oRH87O0clF96.9mwWGB"},
		{"action": "set", "hash": "$2b$12$" + strings.Repeat("a", 53)},
		{"action": "set", "hash": sha512Hash + "\n"},
		{"action": "set", "hash": sha512Hash + "\nroot2:x"},
		{"action": "set", "hash": "\n" + sha512Hash},
		{"action": "set", "hash": sha512Hash + "a"},               // 87 characters of hash
		{"action": "set", "hash": sha512Hash[:len(sha512Hash)-1]}, // 85
		{"action": "set", "hash": " " + sha512Hash},
		{"action": "set", "hash": sha512Hash + ":0:99999"},
		{"action": "set", "hash": "!" + sha512Hash},
		{"action": "set", "hash": "$6$" + strings.Repeat("s", 17) + "$" + strings.Repeat("a", 86)}, // salt too long
		{"action": "set", "hash": "$6$rounds=99$salt$" + strings.Repeat("a", 86)},
		{"action": "set", "hash": "$6$sa;lt$" + strings.Repeat("a", 86)},
		{"action": "set", "hash": yescryptHash + "x"},
		{"action": "set", "hash": "$y$j9T$$" + strings.Repeat("a", 43)},                                  // empty salt
		{"action": "set", "hash": "$y$$salt$" + strings.Repeat("a", 43)},                                 // empty params
		{"action": "set", "hash": "$y$" + strings.Repeat("j", 600) + "$salt$" + strings.Repeat("a", 43)}, // longer than MaxHash
		{"action": "set", "hash": yescryptHash, "user": "nuitcloud"},
		{"action": "set", "hash": yescryptHash, "rounds": 5000},
		{"action": "lock", "hash": yescryptHash},
		{"action": "lock", "force": true},
		{"action": "unlock"},
		{"action": "SET", "hash": yescryptHash},
		{"action": []any{"set"}, "hash": yescryptHash},
		{"hash": yescryptHash},
	}
	for _, m := range bad {
		p, err := Parse(m)
		if err == nil {
			t.Errorf("%q was accepted: %+v", m, p)
			continue
		}
		if strings.Contains(err.Error(), "aaaaaaaa") || strings.Contains(err.Error(), sha512Hash[3:]) || strings.Contains(err.Error(), yescryptHash[7:]) {
			t.Errorf("the refusal names the hash: %v", err)
		}
	}
}

// fakeSystem stands in for chpasswd and usermod: it records what each was given and writes the scratch shadow file as they would.
type fakeSystem struct {
	t      *testing.T
	shadow string
	calls  []string
	stdin  [][]byte
	out    string
	err    error
	ignore bool // finish without changing the file
}

func (f *fakeSystem) exec(_ context.Context, stdin []byte, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	f.stdin = append(f.stdin, stdin)
	if f.err != nil || f.ignore {
		return f.out, f.err
	}
	raw, err := os.ReadFile(f.shadow)
	if err != nil {
		f.t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		parts := strings.Split(l, ":")
		if parts[0] != "root" || len(parts) < 2 {
			continue
		}
		switch name {
		case "chpasswd":
			parts[1] = strings.TrimSuffix(strings.TrimPrefix(string(stdin), "root:"), "\n")
		case "usermod":
			if !strings.HasPrefix(parts[1], "!") {
				parts[1] = "!" + parts[1]
			}
		}
		lines[i] = strings.Join(parts, ":")
	}
	if err := os.WriteFile(f.shadow, []byte(strings.Join(lines, "\n")), 0o640); err != nil {
		f.t.Fatal(err)
	}
	return f.out, nil
}

func scratch(t *testing.T, rootField string) (Console, *fakeSystem) {
	t.Helper()
	root := t.TempDir()
	shadow := ShadowPath(root)
	if err := os.MkdirAll(filepath.Dir(shadow), 0o755); err != nil {
		t.Fatal(err)
	}
	text := "root:" + rootField + ":20000:0:99999:7:::\ndaemon:*:20000:0:99999:7:::\nnuitcloud:" + sha512Slash + ":20000:0:99999:7:::\n"
	if err := os.WriteFile(shadow, []byte(text), 0o640); err != nil {
		t.Fatal(err)
	}
	f := &fakeSystem{t: t, shadow: shadow}
	return Console{Root: root, Exec: f.exec}, f
}

func TestSetGivesTheHashOnStdinOnlyAndConfirmsItInShadow(t *testing.T) {
	for _, before := range []string{"", "!", "*", "!" + sha512Slash, sha512Slash} {
		c, f := scratch(t, before)
		p, _ := Parse(map[string]any{"action": "set", "hash": yescryptHash})
		res, err := c.Set(context.Background(), p)
		if err != nil || res != (Result{Action: "set", Scheme: "yescrypt", State: "set"}) {
			t.Fatalf("from %q: %+v %v", before, res, err)
		}
		if len(f.calls) != 1 || f.calls[0] != "chpasswd -e" {
			t.Errorf("ran %q (the hash must never be an argument)", f.calls)
		}
		if string(f.stdin[0]) != "root:"+yescryptHash+"\n" {
			t.Errorf("stdin was %q", f.stdin[0])
		}
		raw, _ := os.ReadFile(f.shadow)
		if !strings.Contains(string(raw), "nuitcloud:"+sha512Slash+":") {
			t.Error("another account's line changed")
		}
	}
}

func TestASetThatDidNotLandFailsWithoutSayingTheHash(t *testing.T) {
	c, f := scratch(t, sha512Slash)
	f.ignore = true
	p, _ := Parse(map[string]any{"action": "set", "hash": sha512Hash})
	_, err := c.Set(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "not the one given") || strings.Contains(err.Error(), "$6$") {
		t.Errorf("%v", err)
	}
}

func TestAChpasswdFailureIsReportedWithTheHashTakenOut(t *testing.T) {
	c, f := scratch(t, "!")
	f.out = "chpasswd: line 1: cannot update password entry for root:" + sha512Hash + "\nchpasswd: error detected, changes ignored; salt $6$Xw3nQm1pZr8tLs0a\n"
	f.err = errors.New("exit status 1 " + sha512Hash)
	p, _ := Parse(map[string]any{"action": "set", "hash": sha512Hash})
	_, err := c.Set(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "chpasswd refused the hash") || !strings.Contains(err.Error(), "changes ignored") {
		t.Fatalf("%v", err)
	}
	for _, leak := range []string{sha512Hash, "Xw3nQm1pZr8tLs0a", sha512Hash[len(sha512Hash)-20:], "$6$"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error carries %q: %v", leak, err)
		}
	}
}

func TestLockLocksAndConfirms(t *testing.T) {
	for _, before := range []string{"", "*", sha512Slash, "!" + sha512Slash} {
		c, f := scratch(t, before)
		res, err := c.Lock(context.Background())
		if err != nil || res != (Result{Action: "lock", State: "locked"}) {
			t.Fatalf("from %q: %+v %v", before, res, err)
		}
		if len(f.calls) != 1 || f.calls[0] != "usermod -L root" || f.stdin[0] != nil {
			t.Errorf("ran %q", f.calls)
		}
		raw, _ := os.ReadFile(f.shadow)
		field, _ := ShadowField(string(raw), "root")
		if !strings.HasPrefix(field, "!") {
			t.Errorf("root is %q", field)
		}
	}
	c, f := scratch(t, sha512Slash)
	f.ignore = true
	if _, err := c.Lock(context.Background()); err == nil || !strings.Contains(err.Error(), "not locked") || strings.Contains(err.Error(), "$6$") {
		t.Errorf("a lock that did not land: %v", err)
	}
	c, f = scratch(t, sha512Slash)
	f.err = errors.New("exit status 6")
	if _, err := c.Lock(context.Background()); err == nil {
		t.Error("a usermod failure was not a failure")
	}
}

func TestAShadowWithoutRootIsAFailure(t *testing.T) {
	c, f := scratch(t, "!")
	if err := os.WriteFile(f.shadow, []byte("daemon:*:1:0:99999:7:::\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	p, _ := Parse(map[string]any{"action": "set", "hash": sha512Hash})
	if _, err := c.Set(context.Background(), p); err == nil {
		t.Error("set confirmed against a file with no root")
	}
}

func TestTheStatesOfAPasswordField(t *testing.T) {
	for field, want := range map[string]string{"": "none", "!": "locked", "!!": "locked", "*": "locked", "!*": "locked", "!" + sha512Hash: "locked",
		sha512Hash: "set", yescryptHash: "set", "x": "set"} {
		if got := State(field); got != want {
			t.Errorf("%q: %s, want %s", field, got, want)
		}
	}
}

// The direct path, as production runs it: the agent's own child process, no shell, no transient unit. A stub chpasswd on PATH records
// its arguments and standard input and writes the scratch shadow file.
func TestTheRealExecDeliversTheHashOnStdinAndNeverInArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	c, f := scratch(t, "!")
	bin := t.TempDir()
	stub := `#!/bin/sh
printf '%s\n' "$*" >"$STUB_DIR/argv"
env >"$STUB_DIR/env"
IFS= read -r line
printf '%s\n' "$line" >"$STUB_DIR/stdin"
hash="${line#root:}"
printf 'root:%s:20000:0:99999:7:::\n' "$hash" >"$STUB_SHADOW"
`
	if err := os.WriteFile(filepath.Join(bin, "chpasswd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_DIR", bin)
	t.Setenv("STUB_SHADOW", f.shadow)
	c.Exec = nil
	p, _ := Parse(map[string]any{"action": "set", "hash": sha512Slash})
	res, err := c.Set(context.Background(), p)
	if err != nil || res.State != "set" {
		t.Fatalf("%+v %v", res, err)
	}
	argv, _ := os.ReadFile(filepath.Join(bin, "argv"))
	stdin, _ := os.ReadFile(filepath.Join(bin, "stdin"))
	env, _ := os.ReadFile(filepath.Join(bin, "env"))
	if string(argv) != "-e\n" || string(stdin) != "root:"+sha512Slash+"\n" {
		t.Errorf("argv %q stdin %q", argv, stdin)
	}
	if strings.Contains(string(env), sha512Slash[3:19]) {
		t.Error("the hash reached the environment")
	}
}

// Opt-in: in a throwaway Debian container as root (never on a real machine), the real chpasswd and usermod against the container's own
// /etc/shadow. NCS_ROOTCONSOLE_REAL=1 go test ./internal/rootconsole -run Real
func TestRealChpasswdAndUsermodInAThrowawayContainer(t *testing.T) {
	if os.Getenv("NCS_ROOTCONSOLE_REAL") != "1" {
		t.Skip("only in a throwaway container")
	}
	if _, err := exec.LookPath("chpasswd"); err != nil || os.Geteuid() != 0 {
		t.Skip("needs root and chpasswd")
	}
	c := Console{}
	for _, h := range []string{sha512Hash, yescryptHash} {
		p, err := Parse(map[string]any{"action": "set", "hash": h})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Set(context.Background(), p); err != nil {
			t.Fatalf("set %s: %v", p.Scheme, err)
		}
	}
	if _, err := c.Lock(context.Background()); err != nil {
		t.Fatalf("lock: %v", err)
	}
}

// The service unit must keep letting the agent write /etc/shadow directly; if it ever gains ProtectSystem, read-only paths or a
// capability limit, the write has to move to a transient unit (as ncs_login's does) and this test says so.
func TestTheAgentsUnitLetsItWriteShadowDirectly(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/nuit-cloud-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, _, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ProtectSystem", "ReadOnlyPaths", "InaccessiblePaths", "ReadOnlyDirectories", "CapabilityBoundingSet", "User", "DynamicUser", "SystemCallFilter":
			t.Errorf("the unit now has %q: chpasswd may no longer write /etc/shadow from the agent's service; use a transient unit", line)
		}
	}
}
