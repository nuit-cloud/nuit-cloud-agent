// Package rootconsole sets or locks root's console password (nuit-cloud/nuit-cloud#194), so a person at a machine's keyboard can log in
// when the machine cannot reach the platform. The platform generates the password, keeps the plaintext only in OpenBao and signs a task
// that carries nothing but its crypt hash; the agent never sees the password.
//
// This is for the console only. It changes nothing about SSH: the ncs_ssh_harden feature's `PermitRootLogin no` stays, so the password
// cannot be used to log in as root over the network.
//
//   - set gives root the hash, as `root:<hash>` on `chpasswd -e`'s standard input (never in its arguments, an environment variable or a
//     file), then reads /etc/shadow and confirms root's password field is exactly that hash.
//   - lock runs `usermod -L root` and confirms root's password field now starts with `!`.
//
// The hash is never printed: not in the task's output, an error or a log, and not in part beyond the scheme's name.
//
// The agent's service unit (deploy/nuit-cloud-agent.service, and the copy the platform's install script writes) has no ProtectSystem,
// ReadOnlyPaths or capability limit, so the agent may write /etc/shadow itself: chpasswd and usermod run directly as the agent's
// children, not in a transient systemd unit (the login feature needs one only because of ProtectHome). A test holds the unit to that.
package rootconsole

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxHash bounds the hash the agent accepts (a SHA-512 crypt hash is at most 123 characters, a yescrypt one about 150).
const MaxHash = 512

var (
	sha512Crypt = regexp.MustCompile(`^\$6\$(rounds=[0-9]{4,9}\$)?[./A-Za-z0-9]{1,16}\$[./A-Za-z0-9]{86}$`)
	yescrypt    = regexp.MustCompile(`^\$y\$[./A-Za-z0-9]+\$[./A-Za-z0-9]{1,86}\$[./A-Za-z0-9]{43}$`)
	// anyHash finds anything shaped like a crypt hash (or the start of one) in a command's output, so it can be taken out.
	anyHash = regexp.MustCompile(`\$[0-9A-Za-z]{1,2}\$[^\s:]*`)
)

// Params are what a root-console task carries.
type Params struct {
	Action string // set or lock
	Hash   string // set only; never print it
	Scheme string // sha512-crypt or yescrypt (set only): the only part of the hash that may be shown
}

// Parse checks a root-console task's params: exactly {action: "set", hash} with hash a SHA-512 crypt or yescrypt hash on one line, or
// exactly {action: "lock"}. Nothing else is accepted. No error names the hash's value.
func Parse(m map[string]any) (Params, error) {
	action, _ := m["action"].(string)
	switch action {
	case "lock":
		for k := range m {
			if k != "action" {
				return Params{}, fmt.Errorf("root-console lock carries only action, not %q", k)
			}
		}
		return Params{Action: "lock"}, nil
	case "set":
		for k := range m {
			if k != "action" && k != "hash" {
				return Params{}, fmt.Errorf("root-console set carries only action and hash, not %q", k)
			}
		}
		hash, ok := m["hash"].(string)
		if !ok || hash == "" {
			return Params{}, errors.New("root-console set needs a hash")
		}
		if len(hash) > MaxHash {
			return Params{}, fmt.Errorf("the hash is longer than %d characters", MaxHash)
		}
		switch {
		case sha512Crypt.MatchString(hash):
			return Params{Action: "set", Hash: hash, Scheme: "sha512-crypt"}, nil
		case yescrypt.MatchString(hash):
			return Params{Action: "set", Hash: hash, Scheme: "yescrypt"}, nil
		}
		return Params{}, errors.New("the hash is not a SHA-512 crypt ($6$) or yescrypt ($y$) hash on one line")
	}
	return Params{}, errors.New(`the root-console action must be "set" or "lock"`)
}

// Exec runs a program (no shell) with stdin as its standard input and returns what it printed. Injectable so tests touch no real machine.
type Exec func(ctx context.Context, stdin []byte, name string, args ...string) (string, error)

// realExec runs the program directly as the agent's child.
func realExec(ctx context.Context, stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Console changes root's password. Its zero value works on the real machine.
type Console struct {
	Root string // prefix for /etc/shadow ("" on a real machine; tests give a scratch directory)
	Exec Exec
}

// Result is what a root-console task reports: never the hash.
type Result struct {
	Action string `json:"action"`
	Scheme string `json:"scheme,omitempty"`
	// State is root's password state afterwards, as the survey reports it (set, locked).
	State string `json:"state"`
}

func (c Console) exec(ctx context.Context, stdin []byte, name string, args ...string) (string, error) {
	if c.Exec != nil {
		return c.Exec(ctx, stdin, name, args...)
	}
	return realExec(ctx, stdin, name, args...)
}

// ShadowPath is the shadow file under root.
func ShadowPath(root string) string { return filepath.Join(root, "/etc/shadow") }

// ShadowField returns a user's password field (the second) from the text of /etc/shadow, and whether the user has a line.
func ShadowField(text, user string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) >= 2 && f[0] == user {
			return f[1], true
		}
	}
	return "", false
}

// State says what a password field means: "none" (empty: no password needed), "locked" (starts with ! or *, which includes a locked
// hash), otherwise "set".
func State(field string) string {
	switch {
	case field == "":
		return "none"
	case strings.HasPrefix(field, "!") || strings.HasPrefix(field, "*"):
		return "locked"
	}
	return "set"
}

func (c Console) rootField() (string, error) {
	raw, err := os.ReadFile(ShadowPath(c.Root))
	if err != nil {
		return "", fmt.Errorf("could not read /etc/shadow to confirm: %w", err)
	}
	field, ok := ShadowField(string(raw), "root")
	if !ok {
		return "", errors.New("/etc/shadow has no line for root")
	}
	return field, nil
}

// scrub takes out of a command's output the hash and anything shaped like one, and bounds it.
func scrub(out, hash string) string {
	if hash != "" {
		out = strings.ReplaceAll(out, hash, "<hash>")
	}
	out = anyHash.ReplaceAllString(out, "<hash>")
	out = strings.TrimSpace(out)
	if len(out) > 512 {
		out = out[:512] + "…"
	}
	return out
}

func failure(what, out string, err error, hash string) error {
	msg := what + ": " + scrub(err.Error(), hash)
	if o := scrub(out, hash); o != "" {
		msg += ": " + o
	}
	return errors.New(msg)
}

// Set gives root the hash and confirms /etc/shadow holds it.
func (c Console) Set(ctx context.Context, p Params) (Result, error) {
	if p.Action != "set" || p.Hash == "" {
		return Result{}, errors.New("not a set")
	}
	line := []byte("root:" + p.Hash + "\n")
	out, err := c.exec(ctx, line, "chpasswd", "-e")
	if err != nil {
		return Result{}, failure("chpasswd refused the hash", out, err, p.Hash)
	}
	field, err := c.rootField()
	if err != nil {
		return Result{}, err
	}
	if field != p.Hash {
		return Result{}, fmt.Errorf("chpasswd finished but root's password in /etc/shadow is not the one given (it is %s)", State(field))
	}
	return Result{Action: "set", Scheme: p.Scheme, State: "set"}, nil
}

// Lock locks root's password and confirms /etc/shadow says so.
func (c Console) Lock(ctx context.Context) (Result, error) {
	out, err := c.exec(ctx, nil, "usermod", "-L", "root")
	if err != nil {
		return Result{}, failure("usermod -L root failed", out, err, "")
	}
	field, err := c.rootField()
	if err != nil {
		return Result{}, err
	}
	if !strings.HasPrefix(field, "!") {
		return Result{}, fmt.Errorf("usermod finished but root's password in /etc/shadow is not locked (it is %s)", State(field))
	}
	return Result{Action: "lock", State: "locked"}, nil
}
