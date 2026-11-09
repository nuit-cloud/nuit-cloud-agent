// Package update lets the agent replace itself with a newer build, safely.
//
// The platform asks for an update with a signed task that carries the version and the SHA-256 of each build, so the
// checksum is vouched for by the platform's signing key and not merely by the https download. The agent downloads its own
// build, checks the checksum, checks the new binary reports the version it was promised, keeps a copy of the current binary,
// replaces itself atomically and asks to be restarted by the service manager. The new version must then prove itself by
// checking in; if it cannot within a few minutes, or keeps restarting without doing so, the previous binary is put back.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/state"
)

// Doer is what sends a download request: the agent's client (which knows the platform's fallback addresses) or a plain http.Client.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

const (
	// MaxBuildBytes bounds a download: a build is about 6 MB, so anything near this is wrong.
	MaxBuildBytes = 100 << 20
	// ConfirmWithin is how long a new version has to check in successfully before it is rolled back.
	ConfirmWithin = 5 * time.Minute
	// MaxStarts is how many times a new version may start without being confirmed before it is rolled back.
	MaxStarts = 3
)

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([.+-][A-Za-z0-9.]+)?$`)
	hexSHA256      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Params are what the signed update task carries.
type Params struct {
	Version string
	SHA256  map[string]string // by architecture: amd64, arm64
}

// ParseParams reads and validates the parameters of an update-agent task.
func ParseParams(params map[string]any) (Params, error) {
	version, _ := params["version"].(string)
	if !versionPattern.MatchString(version) {
		return Params{}, fmt.Errorf("the update names no usable version (%q)", version)
	}
	raw, _ := params["sha256"].(map[string]any)
	p := Params{Version: version, SHA256: map[string]string{}}
	for arch, v := range raw {
		sum, _ := v.(string)
		if !hexSHA256.MatchString(sum) {
			return Params{}, fmt.Errorf("the checksum for %s is not a SHA-256", arch)
		}
		p.SHA256[arch] = sum
	}
	return p, nil
}

// Outcome says what an update did.
type Outcome struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Changed bool   `json:"changed"`
}

// Updater replaces the running binary.
type Updater struct {
	// BaseURL is the platform's address; builds are served at BaseURL/api/agent/download/<name>.
	BaseURL string
	HTTP    Doer
	// ExePath is the running binary, and StateDir the agent's private directory (where the previous binary is kept).
	ExePath  string
	StateDir string
	Version  string
	Arch     string
	State    *state.State
	Now      func() time.Time
	// Probe runs a binary with arguments and returns what it printed; injectable for tests.
	Probe func(ctx context.Context, path string, args ...string) (string, error)
}

func (u Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u Updater) probe(ctx context.Context, path string, args ...string) (string, error) {
	if u.Probe != nil {
		return u.Probe(ctx, path, args...)
	}
	out, err := exec.CommandContext(ctx, path, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// PreviousPath is where the binary being replaced is kept.
func (u Updater) PreviousPath() string {
	return filepath.Join(u.StateDir, "previous", "nuit-cloud-agent")
}

// Apply installs the build named by p. It returns without restarting: the caller reports the outcome first, then restarts.
func (u Updater) Apply(ctx context.Context, p Params) (Outcome, error) {
	out := Outcome{From: u.Version, To: p.Version}
	if p.Version == u.Version {
		return out, nil // already there
	}
	want := p.SHA256[u.Arch]
	if want == "" {
		return out, fmt.Errorf("the update carries no checksum for this machine's CPU (%s)", u.Arch)
	}
	if u.State.Pending() != nil {
		return out, errors.New("a previous update has not been confirmed yet; not starting another")
	}

	dir := filepath.Dir(u.ExePath)
	next, err := os.CreateTemp(dir, ".nuit-cloud-agent.new-")
	if err != nil {
		return out, fmt.Errorf("cannot stage the new build next to %s: %w", u.ExePath, err)
	}
	staged := next.Name()
	defer os.Remove(staged) // a no-op once it has been renamed into place

	got, err := u.download(ctx, next)
	next.Close()
	if err != nil {
		return out, err
	}
	if got != want {
		return out, fmt.Errorf("the download does not match the checksum the platform signed (expected %s, got %s); nothing was changed", want, got)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return out, err
	}
	reported, err := u.probe(ctx, staged, "version")
	if err != nil {
		return out, fmt.Errorf("the new build does not run on this machine: %w", err)
	}
	if reported != p.Version {
		return out, fmt.Errorf("the new build says it is version %q, not the %q that was promised; nothing was changed", reported, p.Version)
	}

	// The new build must be able to start here, read this machine's identity and reach the platform: a build that cannot do that
	// could not roll itself back either, so it is found out before anything is replaced.
	if said, err := u.probe(ctx, staged, "selftest", "--state-dir", u.StateDir); err != nil || said != "ok" {
		return out, fmt.Errorf("the new build failed its self-test on this machine (%v %q); nothing was changed", err, said)
	}

	// Keep what is being replaced, so a version that cannot check in can be undone.
	if err := copyFile(u.ExePath, u.PreviousPath(), 0o755); err != nil {
		return out, fmt.Errorf("cannot keep the current build to go back to: %w", err)
	}
	if err := u.State.SetPending(state.PendingUpdate{From: u.Version, To: p.Version, At: u.now(), Previous: u.PreviousPath()}); err != nil {
		return out, err
	}
	if err := os.Rename(staged, u.ExePath); err != nil {
		_ = u.State.ClearPending()
		return out, fmt.Errorf("cannot put the new build in place: %w", err)
	}
	out.Changed = true
	return out, nil
}

func (u Updater) download(ctx context.Context, dst io.Writer) (string, error) {
	url := strings.TrimRight(u.BaseURL, "/") + "/api/agent/download/nuit-cloud-agent-linux-" + u.Arch
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := u.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot download the new build: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the platform answered %d when asked for the new build", res.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(res.Body, MaxBuildBytes+1))
	if err != nil {
		return "", fmt.Errorf("the download broke off: %w", err)
	}
	if n > MaxBuildBytes {
		return "", errors.New("the download is far larger than any build; refusing it")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".previous-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Guard watches a freshly installed version and puts the previous one back if it does not prove itself.
type Guard struct {
	ExePath string
	Version string
	State   *state.State
	Now     func() time.Time
}

func (g Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Started is called when the agent starts. It returns true if the version that is running is an unconfirmed update that has
// already started too many times, and so must be rolled back. A pending record for a different version (the agent is back on
// the old one) is forgotten.
func (g Guard) Started() bool {
	p := g.State.Pending()
	if p == nil {
		return false
	}
	if p.To != g.Version {
		_ = g.State.ClearPending()
		return false
	}
	attempts, _ := g.State.BumpPendingAttempts()
	return attempts > MaxStarts
}

// Confirm is called after a successful check-in: the new version has proved itself, so the update stands.
func (g Guard) Confirm() {
	if p := g.State.Pending(); p != nil && p.To == g.Version {
		_ = g.State.ClearPending()
	}
}

// Unconfirmed reports whether this version is an update that has not yet proved itself.
func (g Guard) Unconfirmed() bool {
	p := g.State.Pending()
	return p != nil && p.To == g.Version
}

// Overdue reports whether an unconfirmed update has had its time (ConfirmWithin) and not checked in.
func (g Guard) Overdue(now time.Time) bool {
	p := g.State.Pending()
	return p != nil && p.To == g.Version && now.Sub(p.At) > ConfirmWithin
}

// Rollback puts the previous binary back. The pending record is cleared either way, so a missing previous binary cannot
// leave the agent trying again forever.
func (g Guard) Rollback() error {
	p := g.State.Pending()
	if p == nil {
		return nil
	}
	defer func() { _ = g.State.ClearPending() }()
	if p.Previous == "" {
		return errors.New("there is no previous build to go back to")
	}
	return copyFile(p.Previous, g.ExePath, 0o755)
}
