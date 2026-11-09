// Package profiles runs a profile: a named set of check-and-fix features that makes a machine fit for a role. The features are
// shell scripts in internal/profiles/bootstrap (the only copy; tested by test-bootstrap.sh), embedded in the agent's release, so there is one
// definition of what a profile does whether a person runs it by hand or the agent runs it. A task names a profile; it never
// supplies a script.
package profiles

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed bootstrap
var embedded embed.FS

// Feature is the outcome for one feature of a profile.
type Feature struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, drift (checked, not satisfied), applied, manual (a person must act), failed
	// Notes are what the feature printed about itself (what a person must do, or why it failed).
	Notes []string `json:"notes,omitempty"`
}

// Result is what a profile run reports.
type Result struct {
	Profile      string    `json:"profile"`
	Mode         string    `json:"mode"` // check or apply
	Features     []Feature `json:"features"`
	RebootNeeded bool      `json:"rebootNeeded"`
	// Clean is true when every feature is satisfied: nothing drifted, needs a person, or failed.
	Clean  bool   `json:"clean"`
	Output string `json:"output"`
}

// MaxRawOutput bounds the raw script output kept inside a result (the per-feature results are the useful part).
const MaxRawOutput = 24 * 1024

// Truncate keeps the head and tail of long text and says it cut.
func Truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const marker = "\n… output cut: too long to send …\n"
	half := (limit - len(marker)) / 2
	return text[:half] + marker + text[len(text)-half:]
}

// Runner runs profiles from a set of scripts.
type Runner struct {
	// Scripts is the bootstrap tree (bootstrap.sh, features.sh, profiles/). Defaults to the copy embedded in this binary.
	Scripts fs.FS
	// Command builds the process to run; injectable so tests need no real system.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
	// Euid is the effective user id; applying a profile needs root.
	Euid func() int
}

func (r Runner) scripts() fs.FS {
	if r.Scripts != nil {
		return r.Scripts
	}
	sub, _ := fs.Sub(embedded, "bootstrap")
	return sub
}

// Names lists the profiles this agent carries.
func (r Runner) Names() []string {
	entries, err := fs.ReadDir(r.scripts(), "profiles")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sh") {
			names = append(names, strings.TrimSuffix(e.Name(), ".sh"))
		}
	}
	sort.Strings(names)
	return names
}

// ErrNeedsRoot is returned when a profile is applied by an agent that is not root.
var ErrNeedsRoot = errors.New("applying a profile needs root, and the agent is not running as root")

// Run checks (change nothing) or applies a profile. The exit status of the scripts is returned with the result: 0 means every
// feature is satisfied. env is added to the scripts' environment (the runtime's staged files, when the task carries them).
func (r Runner) Run(ctx context.Context, profile string, apply bool, env ...string) (Result, int, error) {
	known := false
	for _, n := range r.Names() {
		known = known || n == profile
	}
	if !known {
		return Result{}, 0, fmt.Errorf("this agent does not carry a profile named %q (it has: %s)", profile, strings.Join(r.Names(), ", "))
	}
	euid := os.Geteuid
	if r.Euid != nil {
		euid = r.Euid
	}
	if apply && euid() != 0 {
		return Result{}, 0, ErrNeedsRoot
	}

	dir, err := os.MkdirTemp("", "nuit-cloud-profile-")
	if err != nil {
		return Result{}, 0, err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return Result{}, 0, err
	}
	if err := extract(r.scripts(), dir); err != nil {
		return Result{}, 0, err
	}

	args := []string{filepath.Join(dir, "bootstrap.sh"), profile}
	mode := "apply"
	if !apply {
		args = append(args, "--check")
		mode = "check"
	}
	command := r.Command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, "sh", args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, runErr := cmd.CombinedOutput()
	exit := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		} else {
			return Result{}, 0, runErr // could not start it at all
		}
	}
	res := Parse(string(out))
	res.Profile, res.Mode = profile, mode
	res.Output = Truncate(string(out), MaxRawOutput)
	res.Clean = exit == 0
	return res, exit, nil
}

func extract(src fs.FS, dir string) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}

// Parse reads bootstrap.sh's output: one status line per feature (`ok      name`, `DRIFT   name`, `done    name`,
// `MANUAL  name`, `FAILED  name`), `applying name ...` before a feature is made, and indented lines of what a feature said.
func Parse(output string) Result {
	var res Result
	var pending []string // indented notes seen since the last status line
	sc := bufio.NewScanner(strings.NewReader(output))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "  "):
			pending = append(pending, strings.TrimSpace(line))
		case strings.HasPrefix(line, "applying "):
			// the feature is about to be made; its notes and final status follow
		case strings.Contains(line, "A reboot is needed"):
			res.RebootNeeded = true
		default:
			status, name, ok := featureLine(line)
			if !ok {
				continue
			}
			f := Feature{Name: name, Status: status}
			if status != "ok" && len(pending) > 0 {
				f.Notes = pending
			}
			pending = nil
			res.Features = append(res.Features, f)
		}
	}
	if res.Features == nil {
		res.Features = []Feature{}
	}
	return res
}

var statusWords = map[string]string{"ok": "ok", "DRIFT": "drift", "done": "applied", "MANUAL": "manual", "FAILED": "failed"}

func featureLine(line string) (status, name string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", "", false
	}
	s, known := statusWords[fields[0]]
	if !known || path.Base(fields[1]) != fields[1] {
		return "", "", false
	}
	return s, fields[1], true
}
