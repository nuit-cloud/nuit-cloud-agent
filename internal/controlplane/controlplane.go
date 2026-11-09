// Package controlplane is the agent's half of the `nuit-cloud-control-plane` profile: what a control-plane host runs for the platform
// (the scheduler, the agent's https front, a dyndns probe) as native services started by systemd at boot, with no platform needed and no
// container engine.
//
// The signed task names the services and the bundle (version, architecture, SHA-256) that holds the scheduler's program and scripts and
// the probe's script, and nothing secret. The bundle is downloaded, checked against the signed checksum and unpacked here, by a hardened
// extractor (bundle.go). The secrets (the scheduler's tokens, the https front's key, the probe's tokens) come from a second, authenticated
// read of the platform (`GET /api/agent/control-plane-config`) and are written HERE, by the agent, to root-only files. The shell features
// of the profile (internal/profiles/bootstrap, `ncs_cp_*`) install and run what was unpacked; they are only ever told where things are and non-secret
// facts, as environment. No secret is ever passed to a child process, put in a command line, logged or reported.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/mgjoyce/nuit-cloud-agent/internal/client"
)

// Services is the closed list of what a control-plane host may be asked to run, in the order they are set up.
var Services = []string{"scheduler", "tls-front", "probe"}

// SecretName is the platform-tenant secret that holds a service's configuration; the platform lists these (names only) as `missing`.
var SecretName = map[string]string{
	"scheduler": "control-plane/scheduler",
	"tls-front": "control-plane/agent-tls",
	"probe":     "control-plane/probe",
}

// DefaultDir is where the secrets are kept on a control-plane host.
const DefaultDir = "/etc/nuit-cloud/control-plane"

// ConfigPath is the platform's address for the configuration.
const ConfigPath = "/api/agent/control-plane-config"

// MaxConfigBytes bounds what is read from the platform.
const MaxConfigBytes = 256 << 10

// hostArch is this machine's architecture as the platform names it; a variable so tests can stand in for another machine.
var hostArch = func() string { return runtime.GOARCH }

// Params are what a signed task carries: which services, and the bundle they run from. Nothing secret.
type Params struct {
	Services []string
	Bundle   Bundle
}

// NeedsBundle says whether any requested service runs from the bundle (the https front is Debian's own nginx).
func (p Params) NeedsBundle() bool {
	for _, s := range p.Services {
		if s == "scheduler" || s == "probe" {
			return true
		}
	}
	return false
}

// ParseParams checks the `controlPlane` parameter exactly as the platform does: only `services` and `bundle`; services a non-empty,
// unique subset of the closed list; the bundle with exactly version, arch (amd64 or arm64, and this machine's own) and sha256.
func ParseParams(raw map[string]any) (Params, error) {
	for k := range raw {
		if k != "services" && k != "bundle" {
			return Params{}, errors.New("controlPlane may only carry services and bundle")
		}
	}
	list, ok := raw["services"].([]any)
	if !ok || len(list) < 1 {
		return Params{}, fmt.Errorf("services must be one or more of: %s", strings.Join(Services, ", "))
	}
	wanted := map[string]bool{}
	for _, v := range list {
		s, ok := v.(string)
		if !ok || !known(s) || wanted[s] {
			return Params{}, fmt.Errorf("services must be one or more of: %s", strings.Join(Services, ", "))
		}
		wanted[s] = true
	}
	b, ok := raw["bundle"].(map[string]any)
	if !ok {
		return Params{}, errors.New("bundle must give the version, arch and sha256 of the control-plane bundle")
	}
	for k := range b {
		if k != "version" && k != "arch" && k != "sha256" {
			return Params{}, errors.New("bundle may only carry version, arch and sha256")
		}
	}
	str := func(k string) string { s, _ := b[k].(string); return s }
	p := Params{Bundle: Bundle{Version: str("version"), Arch: str("arch"), SHA256: str("sha256")}}
	if !versionRe.MatchString(p.Bundle.Version) {
		return Params{}, errors.New("the bundle's version is not usable")
	}
	if p.Bundle.Arch != "amd64" && p.Bundle.Arch != "arm64" {
		return Params{}, errors.New("the bundle's arch must be amd64 or arm64")
	}
	if p.Bundle.Arch != hostArch() {
		return Params{}, fmt.Errorf("the bundle is for %s and this machine is %s", p.Bundle.Arch, hostArch())
	}
	if !hexSHA256.MatchString(p.Bundle.SHA256) {
		return Params{}, errors.New("the bundle's sha256 is not a SHA-256")
	}
	for _, s := range Services { // the canonical order, whatever order the task listed them in
		if wanted[s] {
			p.Services = append(p.Services, s)
		}
	}
	return p, nil
}

func known(s string) bool {
	for _, k := range Services {
		if k == s {
			return true
		}
	}
	return false
}

// Config is what the platform hands a control-plane host. Everything in it except Revision, Missing and TLSFront.Upstream is secret.
type Config struct {
	Revision  string
	Scheduler *struct{ Env map[string]string }
	TLSFront  *TLSFront
	Probe     *Probe
	Missing   []string
}

// TLSFront is the https front's certificate and key. Upstream (optional, not secret) is where it forwards /api/agent/ to.
type TLSFront struct {
	CertPEM  string
	KeyPEM   string
	Upstream string
}

// Probe is the platform tenant's dyndns probe.
type Probe struct {
	Name           string
	UpdateToken    string
	BootstrapToken string
}

// wire is the platform's JSON.
type wire struct {
	Revision  string `json:"revision"`
	Scheduler *struct {
		Env map[string]any `json:"env"`
	} `json:"scheduler"`
	TLSFront *struct {
		CertPEM  string `json:"certPem"`
		KeyPEM   string `json:"keyPem"`
		Upstream string `json:"upstream"`
	} `json:"tlsFront"`
	Probe *struct {
		Name           string `json:"name"`
		UpdateToken    string `json:"updateToken"`
		BootstrapToken string `json:"bootstrapToken"`
	} `json:"probe"`
	Missing []any `json:"missing"`
}

// Source is the platform: the agent's client.
type Source interface {
	GetJSON(ctx context.Context, path string, maxBytes int64, out any) error
}

var (
	hexSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envName     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	probeName   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	probeToken  = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,512}$`)
	certPEM     = regexp.MustCompile(`^-----BEGIN CERTIFICATE-----[\s\S]+-----END CERTIFICATE-----\s*$`)
	keyPEM      = regexp.MustCompile(`^-----BEGIN (?:EC |RSA )?PRIVATE KEY-----[\s\S]+-----END (?:EC |RSA )?PRIVATE KEY-----\s*$`)
	upstreamRe  = regexp.MustCompile(`^((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9]):[0-9]{1,5}$`)
	maxEnvVars  = 128
	maxEnvValue = 4096
)

// Fetch reads the configuration. It never puts what it read in an error. A refusal is said in the platform's terms: 403 is a machine that is
// not a control-plane host, 503 is a failure that passes (try again later).
func Fetch(ctx context.Context, src Source, wanted []string) (Config, error) {
	var w wire
	if err := src.GetJSON(ctx, ConfigPath, MaxConfigBytes, &w); err != nil {
		var se *client.StatusError
		if errors.As(err, &se) && se.Status == http.StatusForbidden {
			return Config{}, errors.New("this machine is not a control-plane host: the platform will not give it the services' configuration")
		}
		if errors.As(err, &se) && se.Status == http.StatusServiceUnavailable {
			return Config{}, errors.New("the platform cannot read the services' configuration right now; this passes, try again later (retryable)")
		}
		if errors.Is(err, client.ErrTooLarge) {
			return Config{}, errors.New("the platform's configuration answer is larger than this agent will read")
		}
		return Config{}, fmt.Errorf("could not read the services' configuration from the platform: %w", err)
	}
	return parse(w, wanted)
}

func parse(w wire, wanted []string) (Config, error) {
	if !hexSHA256.MatchString(w.Revision) {
		return Config{}, errors.New("the platform's configuration answer has no usable revision")
	}
	c := Config{Revision: w.Revision}
	for _, m := range w.Missing {
		if s, ok := m.(string); ok && isSecretName(s) {
			c.Missing = append(c.Missing, s)
		}
	}
	want := map[string]bool{}
	for _, s := range wanted {
		want[s] = true
	}
	if want["scheduler"] && w.Scheduler != nil {
		env, err := parseEnv(w.Scheduler.Env)
		if err != nil {
			return Config{}, err
		}
		c.Scheduler = &struct{ Env map[string]string }{env}
	}
	if want["tls-front"] && w.TLSFront != nil {
		if len(w.TLSFront.CertPEM) > 65536 || len(w.TLSFront.KeyPEM) > 65536 || !certPEM.MatchString(w.TLSFront.CertPEM) || !keyPEM.MatchString(w.TLSFront.KeyPEM) {
			return Config{}, errors.New("the https front's certificate or key from the platform is not usable")
		}
		t := &TLSFront{CertPEM: w.TLSFront.CertPEM, KeyPEM: w.TLSFront.KeyPEM}
		if validUpstream(w.TLSFront.Upstream) {
			t.Upstream = w.TLSFront.Upstream
		}
		c.TLSFront = t
	}
	if want["probe"] && w.Probe != nil {
		if !probeName.MatchString(w.Probe.Name) || !probeToken.MatchString(w.Probe.UpdateToken) || !probeToken.MatchString(w.Probe.BootstrapToken) {
			return Config{}, errors.New("the probe's name or tokens from the platform are not usable")
		}
		c.Probe = &Probe{Name: w.Probe.Name, UpdateToken: w.Probe.UpdateToken, BootstrapToken: w.Probe.BootstrapToken}
	}
	// a service that was asked for and has neither a section nor a `missing` entry is missing all the same
	for _, s := range wanted {
		have := (s == "scheduler" && c.Scheduler != nil) || (s == "tls-front" && c.TLSFront != nil) || (s == "probe" && c.Probe != nil)
		if !have && !contains(c.Missing, SecretName[s]) {
			c.Missing = append(c.Missing, SecretName[s])
		}
	}
	return c, nil
}

func isSecretName(s string) bool {
	for _, n := range SecretName {
		if n == s {
			return true
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// parseEnv re-checks the scheduler's environment. The values are written quoted (see QuoteEnv), which can hold anything but a control
// character or a newline: such a value is refused, by variable name only.
func parseEnv(raw map[string]any) (map[string]string, error) {
	if len(raw) == 0 || len(raw) > maxEnvVars {
		return nil, errors.New("the scheduler's environment from the platform is empty or too large")
	}
	out := map[string]string{}
	for name, v := range raw {
		if !envName.MatchString(name) {
			return nil, errors.New("the scheduler's environment from the platform has a variable with a name that is not allowed")
		}
		s, ok := v.(string)
		if !ok || len(s) > maxEnvValue || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("the scheduler's variable %s cannot be written to an environment file (not text, too long, or holding a control character)", name)
		}
		out[name] = s
	}
	return out, nil
}

// QuoteEnv writes one line of an environment file for systemd's EnvironmentFile=: NAME="value", with backslash and double quote
// escaped. systemd expands neither `$` nor `%` there, so they stand for themselves, and a trailing backslash is escaped like any other.
func QuoteEnv(name, value string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return name + `="` + r.Replace(value) + `"` + "\n"
}

func validUpstream(s string) bool {
	if !upstreamRe.MatchString(s) {
		return false
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	ip := net.ParseIP(host)
	return err == nil && n >= 1 && n <= 65535 && ip != nil && ip.IsPrivate()
}

// Provisioner prepares a control-plane host for the profile's scripts.
type Provisioner struct {
	Source Source
	// Fetcher downloads the bundle (the agent's runtime stager, so the platform's name and fallback addresses apply).
	Fetcher Fetcher
	// Dir holds the secrets (root-only); defaults to DefaultDir.
	Dir string
	// InstallDir is where each bundle version is unpacked; defaults to DefaultInstallDir.
	InstallDir string
	// StageDir is where a download is kept until it has been checked and unpacked (the agent's state directory).
	StageDir string
	// PlatformURL is the platform's address, which the probe reports to (not secret).
	PlatformURL string
}

func (p Provisioner) dir() string {
	if p.Dir != "" {
		return p.Dir
	}
	return DefaultDir
}

func (p Provisioner) installDir() string {
	if p.InstallDir != "" {
		return p.InstallDir
	}
	return DefaultInstallDir
}

// Prepare reads the platform's configuration and, for an apply only, unpacks the bundle and writes the files the services read (only
// those whose content changed). It returns the non-secret environment for the scripts and a function that removes every secret value from a
// text (a last guard: nothing the scripts print should contain one, and a result must not).
func (p Provisioner) Prepare(ctx context.Context, params Params, apply bool) (env []string, scrub func(string) string, err error) {
	scrub = func(s string) string { return s }
	if p.Source == nil {
		return nil, scrub, errors.New("this agent cannot read the platform's services configuration")
	}
	cfg, err := Fetch(ctx, p.Source, params.Services)
	if err != nil {
		return nil, scrub, err
	}
	scrub = scrubber(cfg)
	dir := p.dir()
	if apply {
		if params.NeedsBundle() {
			if p.Fetcher == nil {
				return nil, scrub, errors.New("this agent cannot download the control-plane bundle")
			}
			if err := Install(ctx, p.Fetcher, params.Bundle, p.installDir(), p.StageDir); err != nil {
				return nil, scrub, fmt.Errorf("could not install the control-plane bundle: %s", scrub(err.Error()))
			}
		}
		if err := write(dir, cfg, params.Services, p.PlatformURL); err != nil {
			return nil, scrub, fmt.Errorf("could not write the services' configuration under %s: %s", dir, scrub(err.Error()))
		}
	}
	var missing []string
	for _, s := range params.Services {
		if contains(cfg.Missing, SecretName[s]) {
			missing = append(missing, SecretName[s])
		}
	}
	sort.Strings(missing)
	env = []string{
		"NCS_CP_SERVICES=" + strings.Join(params.Services, " "),
		"NCS_CP_CONFIG_DIR=" + dir,
		"NCS_CP_INSTALL_DIR=" + p.installDir(),
		"NCS_CP_REVISION=" + cfg.Revision,
		"NCS_CP_MISSING=" + strings.Join(missing, " "),
		"NCS_CP_BUNDLE_VERSION=" + params.Bundle.Version,
		"NCS_CP_BUNDLE_SHA256=" + params.Bundle.SHA256,
	}
	if cfg.TLSFront != nil && cfg.TLSFront.Upstream != "" {
		env = append(env, "NCS_CP_TLS_UPSTREAM="+cfg.TLSFront.Upstream)
	}
	return env, scrub, nil
}

// scrubber replaces every secret value (and the whole key) found in a text. Values shorter than 8 characters are left (they would
// mangle ordinary words; the platform's tokens and keys are long).
func scrubber(c Config) func(string) string {
	var secrets []string
	add := func(v string) {
		if len(v) >= 8 {
			secrets = append(secrets, v, strings.TrimSpace(v))
		}
	}
	if c.Scheduler != nil {
		for _, v := range c.Scheduler.Env {
			add(v)
		}
	}
	if c.TLSFront != nil {
		add(c.TLSFront.KeyPEM)
		add(c.TLSFront.CertPEM)
		// the key's body on its own (a script might print it without the armour lines)
		for _, line := range strings.Split(c.TLSFront.KeyPEM, "\n") {
			if !strings.HasPrefix(line, "-----") {
				add(strings.TrimSpace(line))
			}
		}
	}
	if c.Probe != nil {
		add(c.Probe.UpdateToken)
		add(c.Probe.BootstrapToken)
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return func(s string) string {
		for _, v := range secrets {
			if v != "" {
				s = strings.ReplaceAll(s, v, "[redacted]")
			}
		}
		return s
	}
}

// ─── The files ───────────────────────────────────────────────────────────────────────────────────────────────

type file struct {
	path string // relative to the directory
	mode os.FileMode
	data []byte
}

func files(c Config, wanted []string, platformURL string) []file {
	var out []file
	for _, s := range wanted {
		switch s {
		case "scheduler":
			if c.Scheduler != nil {
				names := make([]string, 0, len(c.Scheduler.Env))
				for n := range c.Scheduler.Env {
					names = append(names, n)
				}
				sort.Strings(names)
				var b strings.Builder
				for _, n := range names {
					b.WriteString(QuoteEnv(n, c.Scheduler.Env[n]))
				}
				out = append(out, file{"scheduler.env", 0o600, []byte(b.String())})
			}
		case "tls-front":
			if c.TLSFront != nil {
				out = append(out, file{"tls/cert.pem", 0o644, []byte(withNewline(c.TLSFront.CertPEM))}, file{"tls/key.pem", 0o600, []byte(withNewline(c.TLSFront.KeyPEM))})
			}
		case "probe":
			if c.Probe != nil {
				body := QuoteEnv("NUIT_CLOUD_API_URL", platformURL) + QuoteEnv("DYNDNS_PROBE_NAME", c.Probe.Name) + QuoteEnv("DYNDNS_UPDATE_TOKEN", c.Probe.UpdateToken) + QuoteEnv("DYNDNS_BOOTSTRAP_TOKEN", c.Probe.BootstrapToken)
				out = append(out, file{"probe.env", 0o600, []byte(body)})
			}
		}
	}
	return out
}

func withNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func write(dir string, c Config, wanted []string, platformURL string) error {
	if strings.ContainsFunc(platformURL, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		return errors.New("the platform address is not usable")
	}
	for _, d := range []string{dir, filepath.Join(dir, "tls")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	for _, f := range files(c, wanted, platformURL) {
		if err := writeIfChanged(filepath.Join(dir, f.path), f.data, f.mode); err != nil {
			return err
		}
	}
	// the revision last: it says the files above belong to it
	return writeIfChanged(filepath.Join(dir, "revision"), []byte(c.Revision+"\n"), 0o644)
}

// writeIfChanged leaves a file alone when its content and mode are already right; otherwise it writes a temporary file in the same
// directory, flushes it to disk and renames it into place, so a reader never sees half a file.
func writeIfChanged(path string, data []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		if st, err := os.Stat(path); err == nil && st.Mode().Perm() != mode {
			return os.Chmod(path, mode)
		}
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".new-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
