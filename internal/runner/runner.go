// Package runner is the agent's catalog: the only things it will ever do. Each is a named task from the platform; anything
// else is refused before it gets here. (The platform has its own copy of this list: adding a task means a release of both.)
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/connectivity"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/controlplane"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/fwconfirm"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/hostname"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/ingress"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/lanscan"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/profiles"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/runtimeinstall"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/servicelog"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/sshconfirm"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/survey"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/update"
)

// ProfileRunner checks or applies a profile (profiles.Runner; injectable so tests touch no real system).
type ProfileRunner interface {
	Run(ctx context.Context, profile string, apply bool, env ...string) (profiles.Result, int, error)
}

// SelfUpdater replaces the running agent with a newer build (update.Updater; injectable so tests touch no real binary).
type SelfUpdater interface {
	Apply(ctx context.Context, p update.Params) (update.Outcome, error)
}

// KeyStore is where the agent keeps the task-signing keys it trusts (state.State; injectable for tests).
type KeyStore interface {
	KeyIDs() []string
	TrustKey(k taskverify.Key) (bool, error)
	RetireKey(id, signedBy string) (bool, error)
}

// Runner performs verified tasks.
type Runner struct {
	// Version is the agent's own version, reported by ping.
	Version string
	// Survey reads the machine; injectable so tests need no real machine.
	Survey   func(ctx context.Context) survey.Report
	Profiles ProfileRunner
	// PlatformURL is the platform's address, handed to the network feature as the thing it must still reach from the new address.
	PlatformURL string
	// Runtime stages the Nuit Container Service runtime's files for a profile-apply that carries them; injectable for tests.
	Runtime RuntimeStager
	// ControlPlane gives the nuit-cloud-control-plane profile the secrets it needs (never through the task).
	ControlPlane ControlPlaneProvisioner
	// Ingress gives the nuit-cloud-ingress profile the site files and certificates it writes (never through the task).
	Ingress IngressProvisioner
	Updater SelfUpdater
	// SSHConfirm completes the SSH hardening: re-checks what sshd uses and cancels the rollback timer; injectable for tests.
	SSHConfirm func(ctx context.Context) (sshconfirm.Result, error)
	// FirewallConfirm completes the baseline firewall: checks the table is loaded and cancels the rollback timer; injectable for tests.
	FirewallConfirm func(ctx context.Context) (fwconfirm.Result, error)
	// Scan asks which addresses of a private range answer on the machine's network; injectable for tests.
	Scan func(ctx context.Context, first, last string) (lanscan.Result, error)
	// Smoke runs the runtime's smoke test in a namespace and returns what it printed and its exit status; injectable for tests.
	Smoke func(ctx context.Context, namespace string) (string, int, error)
	// Repair asks the container runtime about, or ends, supervisors that serve no container (action "list" or "end"); injectable for tests.
	Repair func(ctx context.Context, action string) (string, int, error)
	// SetHostname changes the machine's hostname (hostname.Setter; injectable so tests touch no real machine).
	SetHostname func(ctx context.Context, name, address string) (hostname.Result, error)
	// Connectivity checks which endpoints this machine can reach; injectable so tests dial nothing.
	Connectivity func(ctx context.Context, targets []connectivity.Target, version string) []connectivity.Result
	// JournalExec runs journalctl for service-log (no shell); injectable so tests read no real journal.
	JournalExec servicelog.Exec
	// Keys holds the trusted task-signing keys, changed by trust-key and retire-key.
	Keys KeyStore
	// Restart is set when a task has changed the agent itself (an update), so the loop restarts it after reporting.
	Restart *atomic.Bool
	Now     func() time.Time
}

// RestartRequested reports whether the agent must restart to finish what a task did.
func (r Runner) RestartRequested() bool { return r.Restart != nil && r.Restart.Load() }

// Names is the catalog: what this agent can do.
func (r Runner) Names() []string {
	return []string{"ping", "survey", "profile-check", "profile-apply", "update-agent", "runtime-smoke", "address-scan", "ssh-confirm", "firewall-confirm", "runtime-repair", "set-hostname", "connectivity-check", "service-log", "trust-key", "retire-key"}
}

// Run performs one task, bounded by its timeout, and says how it went. It never panics the agent: a task that blows up is a
// failed task.
func (r Runner) Run(ctx context.Context, task *taskverify.Task) (report client.Report) {
	timeout := time.Duration(task.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			one := 1
			report = client.Report{Status: "failed", ExitCode: &one, Output: fmt.Sprintf("the task panicked: %v", p)}
		}
	}()

	zero, one := 0, 1
	switch task.Name {
	case "ping":
		res := map[string]any{"pong": true, "agentVersion": r.Version, "at": r.now().UTC().Format(time.RFC3339)}
		if r.Keys != nil {
			res["trustedKeyIds"] = r.Keys.KeyIDs()
		}
		return client.Report{Status: "done", ExitCode: &zero, Output: "pong", Result: res}
	case "survey":
		return client.Report{Status: "done", ExitCode: &zero, Output: "surveyed", Result: r.Survey(ctx)}
	case "profile-check", "profile-apply":
		return r.profile(ctx, task)
	case "update-agent":
		return r.selfUpdate(ctx, task)
	case "runtime-smoke":
		return r.smoke(ctx, task)
	case "runtime-repair":
		return r.repair(ctx, task)
	case "set-hostname":
		return r.setHostname(ctx, task)
	case "connectivity-check":
		return r.connectivity(ctx, task)
	case "service-log":
		return r.serviceLog(ctx, task)
	case "address-scan":
		return r.scan(ctx, task)
	case "ssh-confirm":
		return r.sshConfirm(ctx)
	case "firewall-confirm":
		return r.firewallConfirm(ctx)
	case "trust-key":
		return r.trustKey(task)
	case "retire-key":
		return r.retireKey(task)
	}
	return client.Report{Status: "failed", ExitCode: &one, Output: "unknown task " + task.Name}
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// profile checks or applies a profile. A check that finds drift has still done its job (the drift is the answer), so it is
// `done`. An apply is `failed` only if a feature actually failed; a feature that needs a person (a cable, SSH settings, the
// BIOS) is not a failure of the task, so the apply is `done` and the summary and the per-feature results say what is left.
func (r Runner) profile(ctx context.Context, task *taskverify.Task) client.Report {
	one := 1
	name, _ := task.Params["profile"].(string)
	if r.Profiles == nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: "this agent was built without profiles"}
	}
	apply := task.Name == "profile-apply"
	var env []string
	if raw, ok := task.Params["network"].(map[string]any); ok {
		addr, _ := raw["managementAddress"].(string)
		if !ipv4.MatchString(addr) || net.ParseIP(addr) == nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: "the task carries no usable management address"}
		}
		env = append(env, "NCS_MANAGEMENT_ADDRESS="+addr, "NCS_PLATFORM_URL="+r.PlatformURL)
	}
	if raw, ok := task.Params["firewall"].(map[string]any); ok {
		fwEnv, err := firewallEnvironment(raw)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
		}
		env = append(env, fwEnv...)
	}
	if name == "nuit-cloud-wired" {
		// the platform must still be reachable with the wireless interfaces down before the saved network is removed
		env = append(env, "NCS_PLATFORM_URL="+r.PlatformURL)
	}
	if raw, ok := task.Params["login"].(map[string]any); ok {
		key, _ := raw["publicKey"].(string)
		if !loginKey.MatchString(key) {
			return client.Report{Status: "failed", ExitCode: &one, Output: "the task carries no usable ssh-ed25519 public key"}
		}
		env = append(env, "NCS_PLATFORM_SSH_KEY="+key)
	}
	if raw, ok := task.Params["dns"].(map[string]any); ok {
		servers, err := dnsServers(raw)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
		}
		env = append(env, "NCS_DNS_SERVERS="+strings.Join(servers, " "))
	}
	if raw, ok := task.Params["ssh"].(map[string]any); ok {
		fingerprints, err := sshFingerprints(raw)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
		}
		env = append(env, "NCS_SSH_REQUIRE_FINGERPRINTS="+strings.Join(fingerprints, " "))
	}
	if raw, ok := task.Params["vlan"].(map[string]any); ok {
		vlanEnv, err := vlanEnvironment(raw, r.PlatformURL)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
		}
		env = append(env, vlanEnv...)
	}
	if raw, ok := task.Params["runtime"].(map[string]any); ok && apply {
		if r.Runtime == nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: "this agent cannot install the runtime"}
		}
		params, err := runtimeinstall.ParseParams(raw)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
		}
		staged, cleanup, err := r.Runtime.Stage(ctx, params)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: "could not stage the runtime: " + err.Error()}
		}
		defer cleanup()
		env = append(env, staged...)
	} else if ok {
		// a check needs no files: it only asks whether what is installed is healthy
		env = append(env, "NCS_RUNTIME_BUNDLE_SHA256="+stringParam(raw, "bundleSha256"))
	}
	scrub := func(s string) string { return s }
	if raw, ok := task.Params["controlPlane"]; ok || name == controlPlaneProfile {
		if name != controlPlaneProfile {
			return client.Report{Status: "failed", ExitCode: &one, Output: "controlPlane belongs to the " + controlPlaneProfile + " profile only"}
		}
		m, _ := raw.(map[string]any)
		params, err := controlplane.ParseParams(m)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
		}
		if r.ControlPlane == nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: "this agent was built without control-plane support"}
		}
		// The secrets are read here, with the agent's own credential, and written here; the scripts get only where they are.
		cpEnv, cpScrub, err := r.ControlPlane.Prepare(ctx, params, apply)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: cpScrub(err.Error())}
		}
		env, scrub = append(env, cpEnv...), cpScrub
	}
	if raw, ok := task.Params["ingress"]; ok || name == ingress.Profile {
		if name != ingress.Profile {
			return client.Report{Status: "failed", ExitCode: &one, Output: "ingress belongs to the " + ingress.Profile + " profile only"}
		}
		params, err := ingress.ParseParams(raw)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
		}
		if r.Ingress == nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: "this agent was built without ingress support"}
		}
		// The content (site files, certificates, keys) is read here, with the agent's own credential, checked against the signed revision
		// and staged; the scripts get only where it is.
		inEnv, inScrub, cleanup, err := r.Ingress.Prepare(ctx, params, apply)
		if err != nil {
			return client.Report{Status: "failed", ExitCode: &one, Output: inScrub(err.Error())}
		}
		defer cleanup()
		env, scrub = append(env, inEnv...), inScrub
	}
	res, exit, err := r.Profiles.Run(ctx, name, apply, env...)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: scrub(err.Error())}
	}
	res.Output = scrub(res.Output)
	for i := range res.Features {
		for j := range res.Features[i].Notes {
			res.Features[i].Notes[j] = scrub(res.Features[i].Notes[j])
		}
	}
	summary := summarise(res)
	for _, f := range res.Features {
		if apply && f.Status == "failed" {
			return client.Report{Status: "failed", ExitCode: &exit, Output: summary, Result: res}
		}
	}
	if apply && exit != 0 && len(res.Features) == 0 {
		return client.Report{Status: "failed", ExitCode: &exit, Output: summary + " (it produced no result)", Result: res}
	}
	return client.Report{Status: "done", ExitCode: &exit, Output: summary, Result: res}
}

func stringParam(m map[string]any, k string) string { s, _ := m[k].(string); return s }

const controlPlaneProfile = "nuit-cloud-control-plane"

// ControlPlaneProvisioner reads the platform's services configuration and, for an apply, writes it to root-only files
// (controlplane.Provisioner; injectable so tests touch no real machine). It returns the non-secret environment for the scripts and a
// function that removes any secret value from a text.
type ControlPlaneProvisioner interface {
	Prepare(ctx context.Context, p controlplane.Params, apply bool) (env []string, scrub func(string) string, err error)
}

// IngressProvisioner reads and stages the platform's rendered ingress configuration (ingress.Provisioner; injectable for tests). It returns the
// non-secret environment for the scripts, a function that removes any certificate or key from a text, and a cleanup that removes the stage.
type IngressProvisioner interface {
	Prepare(ctx context.Context, p ingress.Params, apply bool) (env []string, scrub func(string) string, cleanup func(), err error)
}

// RuntimeStager downloads and checks the runtime's files (runtimeinstall.Stager).
type RuntimeStager interface {
	Stage(ctx context.Context, p runtimeinstall.Params) (env []string, cleanup func(), err error)
}

// summarise says in a line or two how a profile run went.
func summarise(res profiles.Result) string {
	var satisfied int
	var need []string
	for _, f := range res.Features {
		if f.Status == "ok" || f.Status == "applied" {
			satisfied++
		} else {
			need = append(need, f.Name+" ("+f.Status+")")
		}
	}
	text := fmt.Sprintf("%s %s: %d of %d features satisfied", res.Mode, res.Profile, satisfied, len(res.Features))
	if len(need) > 0 {
		text += "; still to do: " + joinNames(need)
	}
	if res.RebootNeeded {
		text += "; a reboot is needed"
	}
	return text
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// selfUpdate installs the build the (signed) task names. When it replaces the binary it asks to be restarted, which the loop
// does after the result has been reported, so the platform hears of it before the agent goes away.
func (r Runner) selfUpdate(ctx context.Context, task *taskverify.Task) client.Report {
	one, zero := 1, 0
	if r.Updater == nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: "this agent cannot update itself"}
	}
	params, err := update.ParseParams(task.Params)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	outcome, err := r.Updater.Apply(ctx, params)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	if !outcome.Changed {
		return client.Report{Status: "done", ExitCode: &zero, Output: fmt.Sprintf("already at %s", outcome.To), Result: outcome}
	}
	if r.Restart != nil {
		r.Restart.Store(true)
	}
	return client.Report{Status: "done", ExitCode: &zero, Output: fmt.Sprintf("updated from %s to %s; restarting to start the new version", outcome.From, outcome.To), Result: outcome}
}

var ipv4 = regexp.MustCompile(`^([0-9]{1,3}\.){3}[0-9]{1,3}$`)

var smokeNamespace = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// SmokePath is where the runtime feature keeps the bundle's smoke test (copied from the verified bundle when it is installed).
const SmokePath = "/usr/local/lib/nuit-cloud/oci-saas-smoke.sh"

// RunSmoke is the real smoke test: the script the runtime's install left on the machine, run as root.
func RunSmoke(ctx context.Context, namespace string) (string, int, error) {
	if _, err := os.Stat(SmokePath); err != nil {
		return "", 0, fmt.Errorf("the runtime's smoke test is not on this machine (%s): apply the nuit-container-service profile first", SmokePath)
	}
	out, err := exec.CommandContext(ctx, "bash", SmokePath, namespace).CombinedOutput()
	exit := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit, err = ee.ExitCode(), nil
	}
	return string(out), exit, err
}

// The runtime's own program and state root on a machine the Nuit Container Service runtime is installed on.
const (
	RuntimeBinary = "/usr/local/bin/oci-saas"
	RuntimeRoot   = "/var/lib/oci-saas/state"
)

// RunRepair is the real thing. The agent decides nothing about what is a leftover: that judgement is the runtime's own
// (`leftovers` lists the supervisors it finds serving no container, ending none; `reconcile` is its startup sweep, which ends
// those and nothing else that is alive), so the task can only ask the runtime to do what it already does by itself.
func RunRepair(ctx context.Context, action string) (string, int, error) {
	verb := map[string]string{"list": "leftovers", "end": "reconcile"}[action]
	if verb == "" {
		return "", 0, fmt.Errorf("unknown repair action %q", action)
	}
	if _, err := os.Stat(RuntimeBinary); err != nil {
		return "", 0, fmt.Errorf("the container runtime is not on this machine (%s)", RuntimeBinary)
	}
	out, err := exec.CommandContext(ctx, RuntimeBinary, "--root", RuntimeRoot, verb).CombinedOutput()
	exit := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit, err = ee.ExitCode(), nil
	}
	return string(out), exit, err
}

// repair lists, or ends, the supervisors the runtime finds serving no container (see RunRepair). The only parameter is the
// action; an end reports what the runtime says it ended.
func (r Runner) repair(ctx context.Context, task *taskverify.Task) client.Report {
	one := 1
	action, _ := task.Params["action"].(string)
	if action != "list" && action != "end" {
		return client.Report{Status: "failed", ExitCode: &one, Output: `the repair action must be "list" or "end"`}
	}
	run := r.Repair
	if run == nil {
		run = RunRepair
	}
	out, exit, err := run(ctx, action)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	status := "done"
	if exit != 0 {
		status = "failed"
	}
	res := map[string]any{"action": action}
	var parsed map[string]any
	if json.Unmarshal([]byte(out), &parsed) == nil {
		res["runtime"] = parsed
	}
	return client.Report{Status: status, ExitCode: &exit, Output: profiles.Truncate(out, 32*1024), Result: res}
}

// setHostname re-validates the platform's parameters (one DNS label, an optional unicast IPv4 address, nothing else) and
// changes the hostname.
func (r Runner) setHostname(ctx context.Context, task *taskverify.Task) client.Report {
	one := 1
	name, address, err := hostname.Validate(task.Params)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	set := r.SetHostname
	if set == nil {
		set = hostname.Setter{}.Set
	}
	res, err := set(ctx, name, address)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	zero := 0
	return client.Report{Status: "done", ExitCode: &zero, Output: fmt.Sprintf("hostname %s: %s (was %s, by %s)", res.Hostname, res.Outcome, res.Previous, res.Method),
		Result: map[string]any{"previous": res.Previous, "hostname": res.Hostname, "outcome": res.Outcome, "method": res.Method, "address": res.Address, "hostsLine": res.HostsLine}}
}

// connectivity answers which of the named endpoints this machine can reach. It is read-only; the params are validated again here.
func (r Runner) connectivity(ctx context.Context, task *taskverify.Task) client.Report {
	one := 1
	targets, err := connectivity.Validate(task.Params)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	run := r.Connectivity
	if run == nil {
		run = connectivity.Run
	}
	results := run(ctx, targets, r.Version)
	reached := 0
	for _, x := range results {
		if x.OK {
			reached++
		}
	}
	zero := 0
	return client.Report{Status: "done", ExitCode: &zero, Output: fmt.Sprintf("%d of %d reachable", reached, len(results)), Result: map[string]any{"results": results}}
}

// serviceLog returns journal lines of one of the Control Plane Server's own services. It is read-only; params are validated again here.
func (r Runner) serviceLog(ctx context.Context, task *taskverify.Task) client.Report {
	one := 1
	q, err := servicelog.Validate(task.Params, r.now())
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	run := r.JournalExec
	if run == nil {
		run = journalExec
	}
	res := servicelog.Run(ctx, run, q)
	zero := 0
	out := fmt.Sprintf("%d lines of %s", len(res.Lines), q.Key)
	if res.Note != "" {
		out = res.Note
	}
	return client.Report{Status: "done", ExitCode: &zero, Output: out, Result: res}
}

// journalExec runs the command with exec, never a shell.
func journalExec(ctx context.Context, argv []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// smoke runs the runtime's smoke test. It passes only if the script says so (SMOKE OK) and exits 0.
func (r Runner) smoke(ctx context.Context, task *taskverify.Task) client.Report {
	one := 1
	ns, _ := task.Params["namespace"].(string)
	if !smokeNamespace.MatchString(ns) {
		return client.Report{Status: "failed", ExitCode: &one, Output: "the smoke test needs a namespace name"}
	}
	run := r.Smoke
	if run == nil {
		run = RunSmoke
	}
	out, exit, err := run(ctx, ns)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	passed := exit == 0 && strings.Contains(out, "SMOKE OK")
	res := map[string]any{"namespace": ns, "passed": passed}
	status := "done"
	if !passed {
		status = "failed"
	}
	return client.Report{Status: status, ExitCode: &exit, Output: profiles.Truncate(out, 32*1024), Result: res}
}

// scan asks which addresses of the range answer. It changes nothing; an answer is a fact about the network, not a judgement.
func (r Runner) scan(ctx context.Context, task *taskverify.Task) client.Report {
	one, zero := 1, 0
	first, _ := task.Params["first"].(string)
	last, _ := task.Params["last"].(string)
	run := r.Scan
	if run == nil {
		run = lanscan.Real().Scan
	}
	res, err := run(ctx, first, last)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	return client.Report{Status: "done", ExitCode: &zero, Output: fmt.Sprintf("scanned %d addresses from %s (%s): %d answered", res.Scanned, res.From, res.Method, len(res.Answered)), Result: res}
}

// vlanEnvironment turns the signed VLAN slice into what the profile scripts read, after checking every part is what it should be.
func vlanEnvironment(raw map[string]any, platformURL string) ([]string, error) {
	number := func(k string, lo, hi float64) (string, error) {
		f, ok := raw[k].(float64)
		if !ok || f != float64(int(f)) || f < lo || f > hi {
			return "", fmt.Errorf("the VLAN %s is not a whole number from %v to %v", k, lo, hi)
		}
		return fmt.Sprintf("%d", int(f)), nil
	}
	address := func(v any) (string, error) {
		s, _ := v.(string)
		if !ipv4.MatchString(s) || net.ParseIP(s) == nil {
			return "", fmt.Errorf("%q is not an IPv4 address", s)
		}
		return s, nil
	}
	id, err := number("id", 1, 4094)
	if err != nil {
		return nil, err
	}
	prefix, err := number("prefix", 16, 30)
	if err != nil {
		return nil, err
	}
	addr, err := address(raw["address"])
	if err != nil {
		return nil, fmt.Errorf("the VLAN address: %w", err)
	}
	gateway, err := address(raw["gateway"])
	if err != nil {
		return nil, fmt.Errorf("the VLAN gateway: %w", err)
	}
	list, _ := raw["tenantAddresses"].([]any)
	if len(list) < 1 || len(list) > 32 {
		return nil, errors.New("the VLAN carries 1 to 32 tenant addresses")
	}
	var tenants []string
	for _, v := range list {
		a, err := address(v)
		if err != nil {
			return nil, fmt.Errorf("a tenant address: %w", err)
		}
		tenants = append(tenants, a)
	}
	return []string{"NCS_VLAN_ID=" + id, "NCS_VLAN_PREFIX=" + prefix, "NCS_VLAN_ADDRESS=" + addr, "NCS_VLAN_GATEWAY=" + gateway, "NCS_VLAN_TENANT_ADDRESSES=" + strings.Join(tenants, " "), "NCS_PLATFORM_URL=" + platformURL}, nil
}

var sshFingerprint = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// sshFingerprints reads the keys the platform says must still be able to log in, refusing anything that is not a fingerprint.
// firewallEnvironment turns the signed firewall parameters into the scripts' settings: the TCP ports to open to anyone and the ranges
// SSH may come from. Both are validated again here; the platform's own check is not the only one.
var firewallCIDR = regexp.MustCompile(`^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})/(\d{1,2})$`)

func firewallEnvironment(raw map[string]any) ([]string, error) {
	var ports, from []string
	if list, ok := raw["tcp"].([]any); ok {
		if len(list) > 16 {
			return nil, errors.New("the firewall task opens at most 16 TCP ports")
		}
		for _, v := range list {
			n, ok := v.(float64)
			if !ok || n != float64(int(n)) || n < 1 || n > 65535 {
				return nil, fmt.Errorf("%v is not a TCP port", v)
			}
			ports = append(ports, strconv.Itoa(int(n)))
		}
	}
	if list, ok := raw["sshFrom"].([]any); ok {
		if len(list) > 8 {
			return nil, errors.New("the firewall task names at most 8 ranges")
		}
		for _, v := range list {
			c, _ := v.(string)
			m := firewallCIDR.FindStringSubmatch(c)
			if m == nil {
				return nil, fmt.Errorf("%q is not a range such as 10.0.0.0/8", c)
			}
			for _, o := range m[1:5] {
				if n, _ := strconv.Atoi(o); n > 255 {
					return nil, fmt.Errorf("%q is not a range such as 10.0.0.0/8", c)
				}
			}
			if n, _ := strconv.Atoi(m[5]); n > 32 {
				return nil, fmt.Errorf("%q is not a range such as 10.0.0.0/8", c)
			}
			from = append(from, c)
		}
	}
	env := []string{"NCS_FIREWALL_TCP=" + strings.Join(ports, ","), "NCS_FIREWALL_SSH_FROM=" + strings.Join(from, ",")}
	if p, ok := raw["pods"]; ok {
		podEnv, err := podFirewallEnvironment(p)
		if err != nil {
			return nil, fmt.Errorf("the firewall's pod rules: %w", err)
		}
		env = append(env, podEnv...)
	}
	return env, nil
}

// podFirewallEnvironment turns the signed pod rules (nuit-cloud/nuit-cloud#91) into the scripts' settings: the mode (observe counts and logs,
// enforce drops), the destinations pods may reach beyond their own range, and who may reach the platform services' admin ports. Every entry
// is checked here for shape, count and character set before it reaches the script (which checks again); anything the contract does not
// name is refused, so a key the agent does not understand cannot be silently ignored.
func podFirewallEnvironment(v any) ([]string, error) {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("they are not an object")
	}
	for k := range raw {
		if k != "mode" && k != "allow" && k != "admin" {
			return nil, fmt.Errorf("%q is not something the agent knows", k)
		}
	}
	mode, _ := raw["mode"].(string)
	if mode != "observe" && mode != "enforce" {
		return nil, errors.New("the mode is observe or enforce")
	}
	list, ok := raw["allow"].([]any)
	if !ok || len(list) > 32 {
		return nil, errors.New("they allow a list of 0 to 32 destinations")
	}
	allow := make([]string, 0, len(list))
	for _, e := range list {
		s, _ := e.(string)
		if !podDestination(s) {
			return nil, fmt.Errorf("%q is not a destination such as 10.20.0.0/24 or 10.10.10.140:5432", s)
		}
		allow = append(allow, s)
	}
	env := []string{"NCS_FIREWALL_POD_MODE=" + mode, "NCS_FIREWALL_POD_ALLOW=" + strings.Join(allow, ",")}
	a, ok := raw["admin"]
	if !ok {
		return env, nil
	}
	admin, ok := a.(map[string]any)
	if !ok {
		return nil, errors.New("the admin rules are not an object")
	}
	for k := range admin {
		if k != "targets" && k != "ports" && k != "from" {
			return nil, fmt.Errorf("the admin rules: %q is not something the agent knows", k)
		}
	}
	targets, err := podList(admin["targets"], 16, "targets", "an IPv4 address", podAddress)
	if err != nil {
		return nil, err
	}
	from, err := podList(admin["from"], 16, "sources", "an IPv4 address or range", podNetwork)
	if err != nil {
		return nil, err
	}
	pl, ok := admin["ports"].([]any)
	if !ok || len(pl) < 1 || len(pl) > 8 {
		return nil, errors.New("the admin rules name 1 to 8 ports")
	}
	adminPorts := make([]string, 0, len(pl))
	for _, e := range pl {
		n, ok := e.(float64)
		if !ok || n != float64(int(n)) || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%v is not a port", e)
		}
		adminPorts = append(adminPorts, strconv.Itoa(int(n)))
	}
	return append(env, "NCS_FIREWALL_ADMIN_TARGETS="+strings.Join(targets, ","), "NCS_FIREWALL_ADMIN_PORTS="+strings.Join(adminPorts, ","),
		"NCS_FIREWALL_ADMIN_FROM="+strings.Join(from, ",")), nil
}

// podList is a list of 1 to max strings, each accepted by ok.
func podList(v any, max int, plural, what string, ok func(string) bool) ([]string, error) {
	list, isList := v.([]any)
	if !isList || len(list) < 1 || len(list) > max {
		return nil, fmt.Errorf("the admin rules name 1 to %d %s", max, plural)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, _ := e.(string)
		if !ok(s) {
			return nil, fmt.Errorf("%q is not %s", s, what)
		}
		out = append(out, s)
	}
	return out, nil
}

// podAddress is an IPv4 address written plainly (no leading zeros, nothing around it).
func podAddress(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4() && a.String() == s
}

// podNetwork is an IPv4 address or an IPv4 range such as 10.0.0.0/8.
func podNetwork(s string) bool {
	addr, bits, cut := strings.Cut(s, "/")
	if !cut {
		return podAddress(s)
	}
	n, err := strconv.Atoi(bits)
	return podAddress(addr) && err == nil && strconv.Itoa(n) == bits && n >= 0 && n <= 32
}

// podDestination is a podNetwork, optionally with :port (1-65535).
func podDestination(s string) bool {
	host, port, cut := strings.Cut(s, ":")
	if !cut {
		return podNetwork(s)
	}
	n, err := strconv.Atoi(port)
	return podNetwork(host) && err == nil && strconv.Itoa(n) == port && n >= 1 && n <= 65535
}

// dnsServers is the platform's list of resolvers for the machine: one to three IPv4 addresses, nothing else (it goes into a file the machine
// resolves every name through).
func dnsServers(raw map[string]any) ([]string, error) {
	list, _ := raw["servers"].([]any)
	if len(list) < 1 || len(list) > 3 {
		return nil, fmt.Errorf("the task carries no usable resolver list (one to three IPv4 addresses)")
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, _ := v.(string)
		if !ipv4.MatchString(s) || net.ParseIP(s) == nil {
			return nil, fmt.Errorf("the task carries no usable resolver list (one to three IPv4 addresses)")
		}
		out = append(out, s)
	}
	return out, nil
}

// loginKey is one ssh-ed25519 public key line with an optional comment: nothing that could carry options or a second line.
var loginKey = regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/]{68}( [A-Za-z0-9@._-]{1,64})?$`)

func sshFingerprints(raw map[string]any) ([]string, error) {
	list, _ := raw["requireFingerprints"].([]any)
	if len(list) < 1 || len(list) > 10 {
		return nil, errors.New("the SSH task names 1 to 10 keys that must still be able to log in")
	}
	var out []string
	for _, v := range list {
		f, _ := v.(string)
		if !sshFingerprint.MatchString(f) {
			return nil, fmt.Errorf("%q is not a key fingerprint", f)
		}
		out = append(out, f)
	}
	return out, nil
}

// trustKey adds a task-signing key (the first half of a rollover). It reaches here only as a verified task, so only a key the agent already
// trusts can add another; the key is checked again (shape, and that its id is derived from it). Adding a key already trusted changes nothing.
func (r Runner) trustKey(task *taskverify.Task) client.Report {
	one, zero := 1, 0
	if r.Keys == nil || task.SignedBy == "" {
		return client.Report{Status: "failed", ExitCode: &one, Output: "this agent cannot change the keys it trusts"}
	}
	key, err := taskverify.KeyFromParams(task.Params)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error()}
	}
	changed, err := r.Keys.TrustKey(key)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error(), Result: r.keysResult(key.KeyID, false)}
	}
	out := "now trusts key " + key.KeyID
	if !changed {
		out = "already trusted key " + key.KeyID
	}
	return client.Report{Status: "done", ExitCode: &zero, Output: out, Result: r.keysResult(key.KeyID, changed)}
}

// retireKey stops trusting a key (the second half of a rollover), never the one that signed this task and never the last one.
func (r Runner) retireKey(task *taskverify.Task) client.Report {
	one, zero := 1, 0
	if r.Keys == nil || task.SignedBy == "" {
		return client.Report{Status: "failed", ExitCode: &one, Output: "this agent cannot change the keys it trusts"}
	}
	for k := range task.Params {
		if k != "keyId" {
			return client.Report{Status: "failed", ExitCode: &one, Output: fmt.Sprintf("retire-key carries only keyId, not %q", k)}
		}
	}
	id, _ := task.Params["keyId"].(string)
	if !taskverify.ValidKeyID(id) {
		return client.Report{Status: "failed", ExitCode: &one, Output: "the key id must be 16 lower-case hex characters"}
	}
	changed, err := r.Keys.RetireKey(id, task.SignedBy)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error(), Result: r.keysResult(id, false)}
	}
	out := "no longer trusts key " + id
	if !changed {
		out = "key " + id + " was not trusted"
	}
	return client.Report{Status: "done", ExitCode: &zero, Output: out, Result: r.keysResult(id, changed)}
}

func (r Runner) keysResult(id string, changed bool) map[string]any {
	return map[string]any{"keyId": id, "changed": changed, "trustedKeyIds": r.Keys.KeyIDs()}
}

// firewallConfirm completes the baseline firewall once the platform has tested from outside.
func (r Runner) firewallConfirm(ctx context.Context) client.Report {
	one, zero := 1, 0
	run := r.FirewallConfirm
	if run == nil {
		run = fwconfirm.Confirmer{Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		}}.Confirm
	}
	res, err := run(ctx)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error(), Result: res}
	}
	return client.Report{Status: "done", ExitCode: &zero, Output: res.Note, Result: res}
}

// sshConfirm completes the SSH hardening once the platform has tested sshd from outside.
func (r Runner) sshConfirm(ctx context.Context) client.Report {
	one, zero := 1, 0
	run := r.SSHConfirm
	if run == nil {
		run = sshconfirm.Confirmer{Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		}}.Confirm
	}
	res, err := run(ctx)
	if err != nil {
		return client.Report{Status: "failed", ExitCode: &one, Output: err.Error(), Result: res}
	}
	return client.Report{Status: "done", ExitCode: &zero, Output: res.Note, Result: res}
}
