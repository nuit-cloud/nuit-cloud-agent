// Package sshkeyremove takes SSH public keys the platform's registry does not know out of the login account's authorized_keys, by
// fingerprint (nuit-cloud/nuit-cloud#200), so a person never has to log in over SSH to do it.
//
// The task carries public fingerprints only: the keys to remove, the registry's active keys for this machine (never removed) and the
// administrator and break-glass keys (at least one must still be in the file afterwards). Lines are read with the survey's own rules
// (survey.ClassifyAuthorizedKeyLine), so the task and the survey agree on what a key and a platform line are.
//
//   - Only unmarked lines are removed. A line the platform marked `nuit-cloud:<id>` belongs to the login profile (Sync keys) and is never
//     removed here; its fingerprint is reported as managed.
//   - Every other byte of the file stays as it was: comments, blank lines, lines that hold no key, other keys, line endings, and whether
//     the file ends in a newline.
//   - The new file is written beside the old one, given mode 600 and the account as owner, then renamed over it, and only if the file
//     is still exactly what was read. The agent's service has ProtectHome=read-only, so on a machine with systemd the write runs in a
//     transient unit (as the login profile's does); without systemd (tests) it runs directly.
//
// Nothing but fingerprints is ever printed: no key body, no comment.
package sshkeyremove

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/survey"
)

// Limits on each list the task carries.
const (
	MaxFingerprints = 16
	MaxKeep         = 64
	MaxRequireOneOf = 16
)

var (
	fingerprintRe = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	homeRe        = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,254}$`)
	gidRe         = regexp.MustCompile(`^[0-9]{1,10}$`)
	sha256Re      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Params are what an ssh-key-remove task carries.
type Params struct {
	Fingerprints []string // the keys to remove
	Keep         []string // the registry's active keys for this machine: never removed
	RequireOneOf []string // administrator and break-glass keys: at least one must remain
}

// Parse checks an ssh-key-remove task's params: exactly {fingerprints, keep, requireOneOf}, each a list of unique `SHA256:` + 43
// unpadded base64 fingerprints (1 to 16, 1 to 64 and 1 to 16 of them), and no fingerprint to remove also in keep or requireOneOf.
// Nothing else is accepted. No error repeats a value that is not a well-formed fingerprint.
func Parse(m map[string]any) (Params, error) {
	for k := range m {
		if k != "fingerprints" && k != "keep" && k != "requireOneOf" {
			return Params{}, fmt.Errorf("ssh-key-remove carries only fingerprints, keep and requireOneOf, not %q", clip(k))
		}
	}
	var p Params
	var err error
	if p.Fingerprints, err = fingerprintList(m, "fingerprints", MaxFingerprints); err != nil {
		return Params{}, err
	}
	if p.Keep, err = fingerprintList(m, "keep", MaxKeep); err != nil {
		return Params{}, err
	}
	if p.RequireOneOf, err = fingerprintList(m, "requireOneOf", MaxRequireOneOf); err != nil {
		return Params{}, err
	}
	keep, req := set(p.Keep), set(p.RequireOneOf)
	for _, fp := range p.Fingerprints {
		if keep[fp] {
			return Params{}, fmt.Errorf("%s is one of the registry's active keys for this machine (keep): it is never removed; nothing was changed", fp)
		}
		if req[fp] {
			return Params{}, fmt.Errorf("%s is an administrator or break-glass key (requireOneOf): it is not removed this way; nothing was changed", fp)
		}
	}
	return p, nil
}

func fingerprintList(m map[string]any, name string, max int) ([]string, error) {
	raw, present := m[name]
	if !present {
		return nil, fmt.Errorf("ssh-key-remove needs %s", name)
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("%s must be a list of fingerprints", name)
	}
	if len(items) < 1 || len(items) > max {
		return nil, fmt.Errorf("%s must hold 1 to %d fingerprints", name, max)
	}
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for i, it := range items {
		s, ok := it.(string)
		if !ok || !fingerprintRe.MatchString(s) {
			return nil, fmt.Errorf("%s[%d] is not a fingerprint (SHA256: and 43 base64 characters)", name, i)
		}
		if seen[s] {
			return nil, fmt.Errorf("%s lists %s twice", name, s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

func set(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// Result is what an ssh-key-remove task reports: fingerprints only, each list in the order the task gave them. A fingerprint is
// removed when at least one unmarked line held it, managed when a platform-marked line holds it (left in place), notFound when no line
// holds it; one can be both removed and managed.
type Result struct {
	Removed      []string `json:"removed"`
	NotFound     []string `json:"notFound"`
	Managed      []string `json:"managed"`
	LinesRemoved int      `json:"linesRemoved"`
}

// Summary is the task's one-line output.
func (r Result) Summary() string {
	s := fmt.Sprintf("removed %d line(s) holding %d key(s)", r.LinesRemoved, len(r.Removed))
	if len(r.NotFound) > 0 {
		s += fmt.Sprintf("; %d not in the file", len(r.NotFound))
	}
	if len(r.Managed) > 0 {
		s += fmt.Sprintf("; %d on the platform's own lines, left to Sync keys", len(r.Managed))
	}
	return s
}

// Plan works out the new content of an authorized_keys file. It returns the new content, what it would report, and whether anything
// changes. It refuses (an error, nothing to write) when the result would hold none of p.RequireOneOf, or when a line it would remove
// holds a key in p.Keep.
func Plan(content []byte, p Params) ([]byte, Result, bool, error) {
	remove, keep, req := set(p.Fingerprints), set(p.Keep), set(p.RequireOneOf)
	removed, managed, found := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var out bytes.Buffer
	lines := 0
	adminLeft := false
	for _, piece := range bytes.SplitAfter(content, []byte("\n")) {
		if len(piece) == 0 {
			continue
		}
		k, ok := survey.ClassifyAuthorizedKeyLine(string(piece))
		if ok && remove[k.Fingerprint] {
			found[k.Fingerprint] = true
			if k.ManagedID != nil {
				managed[k.Fingerprint] = true
			} else {
				if keep[k.Fingerprint] {
					return nil, Result{}, false, fmt.Errorf("a line to remove holds %s, one of the registry's active keys for this machine: nothing was changed", k.Fingerprint)
				}
				removed[k.Fingerprint] = true
				lines++
				continue
			}
		}
		if ok && req[k.Fingerprint] {
			adminLeft = true
		}
		out.Write(piece)
	}
	res := Result{Removed: []string{}, NotFound: []string{}, Managed: []string{}, LinesRemoved: lines}
	for _, fp := range p.Fingerprints {
		if removed[fp] {
			res.Removed = append(res.Removed, fp)
		}
		if managed[fp] {
			res.Managed = append(res.Managed, fp)
		}
		if !found[fp] {
			res.NotFound = append(res.NotFound, fp)
		}
	}
	if lines == 0 {
		return content, res, false, nil
	}
	if !adminLeft {
		return nil, Result{}, false, errors.New("removing these keys would leave no administrator or break-glass key in authorized_keys: nothing was changed")
	}
	next := out.Bytes()
	// The last line was removed and the file did not end in a newline: neither does the new one.
	if !bytes.HasSuffix(content, []byte("\n")) && bytes.HasSuffix(next, []byte("\n")) {
		next = bytes.TrimSuffix(next, []byte("\n"))
		next = bytes.TrimSuffix(next, []byte("\r"))
	}
	return next, res, true, nil
}

// Writer replaces home/.ssh/authorized_keys with content, owned by user:group, only if the file's SHA-256 (hex) is still sha.
// Injectable on the Remover so unit tests touch no real machine.
type Writer func(ctx context.Context, content []byte, home, user, group, sha string) error

// Remover removes keys. Its zero value works on the real machine.
type Remover struct {
	Root  string // prefix for /etc/passwd and the login account's home ("" on a real machine; tests give a scratch directory)
	Write Writer // nil: the real writer (WriteScript, in a transient systemd unit when the machine has systemd and Root is "")
}

// Remove takes the keys out of the login account's authorized_keys as Plan says, and writes the file only when something changes.
func (r Remover) Remove(ctx context.Context, p Params) (Result, error) {
	passwd, err := os.ReadFile(filepath.Join(r.Root, "/etc/passwd"))
	if err != nil {
		return Result{}, fmt.Errorf("could not read /etc/passwd: %w", err)
	}
	home, gid := survey.LoginEntry(string(passwd))
	if home == "" {
		return Result{}, errors.New("there is no account " + survey.LoginAccount)
	}
	if !homeRe.MatchString(home) || path.Clean(home) != home || strings.Contains(home, "..") || !gidRe.MatchString(gid) {
		return Result{}, errors.New("the account " + survey.LoginAccount + "'s home or group in /etc/passwd is not one the agent will write under")
	}
	dir := filepath.Join(r.Root, home, ".ssh")
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return Result{}, errors.New(home + "/.ssh is a symbolic link: left to a person; nothing was changed")
	}
	file := filepath.Join(dir, "authorized_keys")
	fi, err := os.Lstat(file)
	if errors.Is(err, fs.ErrNotExist) {
		_, res, _, err := Plan(nil, p)
		return res, err
	}
	if err != nil {
		return Result{}, fmt.Errorf("could not read authorized_keys: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Result{}, errors.New(home + "/.ssh/authorized_keys is not a regular file: left to a person; nothing was changed")
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return Result{}, fmt.Errorf("could not read authorized_keys: %w", err)
	}
	next, res, changed, err := Plan(content, p)
	if err != nil || !changed {
		return res, err
	}
	sum := sha256.Sum256(content)
	write := r.Write
	if write == nil {
		systemd := r.Root == "" && hasSystemd()
		write = func(ctx context.Context, content []byte, home, user, group, sha string) error {
			return WriteScript(ctx, systemd, content, home, user, group, sha)
		}
	}
	if err := write(ctx, next, filepath.Join(r.Root, home), survey.LoginAccount, gid, hex.EncodeToString(sum[:])); err != nil {
		return Result{}, err
	}
	return res, nil
}

func hasSystemd() bool {
	if fi, err := os.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// writeScript is the writer: $1 home, $2 user, $3 group, $4 the SHA-256 (hex) the file must still have; the new content on standard
// input. It holds no dollar-brace, percent or doubled dollar, because systemd expands them in a transient unit's command line. It refuses a symbolic-link .ssh or a non-regular authorized_keys, checks the file is still what was read, writes the new
// content to a temporary file in the same directory (so the rename is atomic), gives it mode 600 and the account as owner, and renames
// it over the old file; on any failure the old file stays and the temporary one goes. A chown failure is tolerated only when not root
// (tests).
const writeScript = `home="$1"; user="$2"; grp="$3"; want="$4"; dir="$home/.ssh"; file="$dir/authorized_keys"
[ ! -L "$dir" ] && [ -d "$dir" ] || { echo "$dir is not a plain directory: nothing was changed" >&2; exit 1; }
[ ! -L "$file" ] && [ -f "$file" ] || { echo "$file is not a regular file: nothing was changed" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then have="$(sha256sum <"$file" | cut -d" " -f1)"; else have="$(shasum -a 256 <"$file" | cut -d" " -f1)"; fi
[ -n "$have" ] && [ "$have" = "$want" ] || { echo "authorized_keys changed while the removal was prepared: nothing was changed" >&2; exit 1; }
tmp="$(mktemp "$dir/.authorized_keys.nuit-cloud.XXXXXX")" || { echo "could not make a temporary file in $dir: nothing was changed" >&2; exit 1; }
if cat >"$tmp" && chmod 600 "$tmp" && { chown "$user:$grp" "$tmp" 2>/dev/null || [ "$(id -u)" != 0 ]; } && mv -f "$tmp" "$file"; then
  exit 0
fi
rm -f "$tmp"; echo "could not write authorized_keys: nothing was changed" >&2; exit 1`

// WriteScript runs the writer script with content on its standard input: in a transient systemd unit when systemd is true (outside the
// agent's ProtectHome), else directly with /bin/sh. Every argument is checked first, since systemd would expand a percent or a dollar in
// them too.
func WriteScript(ctx context.Context, systemd bool, content []byte, home, user, group, sha string) error {
	if !strings.HasPrefix(home, "/") || strings.ContainsAny(home, "%$\n") || user != survey.LoginAccount || !gidRe.MatchString(group) || !sha256Re.MatchString(sha) {
		return errors.New("the writer's arguments are not usable: nothing was changed")
	}
	args := []string{"-c", writeScript, "sh", home, user, group, sha}
	name := "/bin/sh"
	if systemd {
		args = append([]string{"--quiet", "--wait", "--collect", "--pipe", "/bin/sh"}, args...)
		name = "systemd-run"
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 512 {
			msg = msg[:512] + "…"
		}
		if msg == "" {
			msg = err.Error() + ": nothing was changed"
		}
		return errors.New(msg)
	}
	return nil
}
