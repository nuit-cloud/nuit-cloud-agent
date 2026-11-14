// Package ingress is the agent's half of the `nuit-cloud-ingress` profile (nuit-cloud/nuit-cloud#184): the machine that plays the Ingress
// Service runs nginx in a container, and the platform's routes become site files and certificates in two directories it reads.
//
// The signed task carries only non-secret facts: the revision the platform wants applied, where the site files go (confDir), under which
// directory the certificates go (certRoot) and which container is tested and reloaded. The content (site files, certificates, private keys)
// is read here, by the agent, with its own credential (`GET /api/agent/ingress-config`), never from the task. What ties that unsigned
// content to the signature is the revision: it is computed again here from the content and must equal the signed one, or nothing is done.
// Every name and directory in the content is checked against the signed settings (a site file is a name in confDir and nothing else; a
// certificate's directory is below certRoot). The content is staged in a root-only directory on tmpfs and the profile's scripts are told
// only where it is; they write it, test it with nginx in the named container and put the previous files back if nginx refuses it.
package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
)

// Profile is the profile that applies the Ingress Service.
const Profile = "nuit-cloud-ingress"

// ConfigPath is the platform's address for the rendered ingress configuration.
const ConfigPath = "/api/agent/ingress-config"

// StateDir is where the profile records what it last applied (`settings` and `revision`); the `ingress` observation reads it.
const StateDir = "/var/lib/nuit-cloud/ingress"

// DefaultStageRoot is where the fetched content is staged: tmpfs, outside the agent's private /tmp, so the profile's writer (a transient
// systemd unit, because the agent's own service may not write under /home) can read it.
const DefaultStageRoot = "/run/nuit-cloud-agent"

// Limits on what is read from the platform (the platform keeps within the same ones).
const (
	MaxConfigBytes = 8 << 20
	MaxSites       = 512
	MaxCerts       = 512
	MaxSiteBytes   = 256 << 10
	MaxPEMBytes    = 64 << 10
	maxPathLen     = 255
)

var (
	revisionRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	siteFileRe  = regexp.MustCompile(`^(00-nuit-cloud-default|nuit-cloud-[a-z0-9-]{1,120})\.conf$`)
	pathRe      = regexp.MustCompile(`^(/[A-Za-z0-9_-][A-Za-z0-9._-]{0,127})+$`)
	// a certificate's directory below certRoot: each part a plain name, or a wildcard's `*.` followed by one (`*.example.com`)
	certPartRe = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9_-][A-Za-z0-9._-]{0,252}$`)
	certPEM    = regexp.MustCompile(`^-----BEGIN CERTIFICATE-----\r?\n[\s\S]+-----END CERTIFICATE-----\s*$`)
	keyPEM     = regexp.MustCompile(`^-----BEGIN (?:EC |RSA )?PRIVATE KEY-----\r?\n[\s\S]+-----END (?:EC |RSA )?PRIVATE KEY-----\s*$`)
)

// Params are what the signed task carries under `ingress`.
type Params struct {
	Revision  string
	ConfDir   string
	CertRoot  string
	Container string
}

// ValidRevision says whether s is a revision: 64 lower-case hex characters.
func ValidRevision(s string) bool { return revisionRe.MatchString(s) }

// ValidContainer says whether s can be a Docker container's name.
func ValidContainer(s string) bool { return containerRe.MatchString(s) }

// ValidSiteFile says whether name is one of the platform's site files (and so one the profile may write, replace or remove).
func ValidSiteFile(name string) bool { return siteFileRe.MatchString(name) }

// ValidPath says whether s is a directory the task may name: absolute, already clean (no `.`, `..`, `//` or trailing slash), each part a
// plain name of letters, digits, `.`, `_` and `-` not starting with a dot, at most 255 characters, and not `/` itself.
func ValidPath(s string) bool {
	return len(s) <= maxPathLen && pathRe.MatchString(s) && filepath.Clean(s) == s
}

// within says whether dir is strictly below root.
func within(dir, root string) bool { return strings.HasPrefix(dir, root+"/") }

// ParseParams checks the `ingress` parameter exactly as the platform does: an object with exactly revision, confDir, certRoot and
// container, each of its shape; the two directories apart (neither inside the other).
func ParseParams(raw any) (Params, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Params{}, errors.New("the task carries no ingress settings (revision, confDir, certRoot, container)")
	}
	for k := range m {
		if k != "revision" && k != "confDir" && k != "certRoot" && k != "container" {
			return Params{}, fmt.Errorf("ingress carries only revision, confDir, certRoot and container, not %q", k)
		}
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	p := Params{Revision: str("revision"), ConfDir: str("confDir"), CertRoot: str("certRoot"), Container: str("container")}
	if !ValidRevision(p.Revision) {
		return Params{}, errors.New("the ingress revision is not 64 lower-case hex characters")
	}
	if !ValidPath(p.ConfDir) {
		return Params{}, errors.New("the ingress confDir is not a plain absolute directory")
	}
	if !ValidPath(p.CertRoot) {
		return Params{}, errors.New("the ingress certRoot is not a plain absolute directory")
	}
	if p.ConfDir == p.CertRoot || within(p.ConfDir, p.CertRoot) || within(p.CertRoot, p.ConfDir) {
		return Params{}, errors.New("the ingress confDir and certRoot must be apart (neither inside the other)")
	}
	if !ValidContainer(p.Container) {
		return Params{}, errors.New("the ingress container is not a container name")
	}
	return p, nil
}

// Site is one site file: its name in confDir and its content.
type Site struct {
	File    string
	Content string
}

// Certificate is one certificate's directory (absolute, below certRoot) and its two files.
type Certificate struct {
	Dir          string
	FullchainPEM string
	PrivkeyPEM   string
}

// Config is the platform's rendered ingress configuration, checked.
type Config struct {
	Revision     string
	Sites        []Site
	Certificates []Certificate
	// Problems is how many routes the platform could not render (and left out); only the count is kept.
	Problems int
}

// Revision is the SHA-256 (hex) the platform and the agent both compute over the content: sites sorted by file and certificates by dir
// (byte order), each site framed as "site\0" file "\0" content "\0", then each certificate as "cert\0" dir "\0" fullchain "\0" key "\0".
func Revision(sites []Site, certs []Certificate) string {
	s := append([]Site(nil), sites...)
	sort.Slice(s, func(i, j int) bool { return s[i].File < s[j].File })
	c := append([]Certificate(nil), certs...)
	sort.Slice(c, func(i, j int) bool { return c[i].Dir < c[j].Dir })
	h := sha256.New()
	for _, x := range s {
		h.Write([]byte("site\x00" + x.File + "\x00" + x.Content + "\x00"))
	}
	for _, x := range c {
		h.Write([]byte("cert\x00" + x.Dir + "\x00" + x.FullchainPEM + "\x00" + x.PrivkeyPEM + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Wire is the platform's JSON.
type Wire struct {
	Revision string `json:"revision"`
	Sites    []struct {
		File    string `json:"file"`
		Content string `json:"content"`
	} `json:"sites"`
	Certificates []struct {
		Dir          string `json:"dir"`
		FullchainPEM string `json:"fullchainPem"`
		PrivkeyPEM   string `json:"privkeyPem"`
	} `json:"certificates"`
	Problems []any `json:"problems"`
}

// Source is the platform: the agent's client.
type Source interface {
	GetJSON(ctx context.Context, path string, maxBytes int64, out any) error
}

// Fetch reads the configuration and checks it against the signed settings. It never puts what it read in an error.
func Fetch(ctx context.Context, src Source, p Params) (Config, error) {
	var w Wire
	if err := src.GetJSON(ctx, ConfigPath, MaxConfigBytes, &w); err != nil {
		var se *client.StatusError
		if errors.As(err, &se) && se.Status == http.StatusForbidden {
			return Config{}, errors.New("this machine is not the Ingress Service: the platform will not give it the ingress configuration")
		}
		if errors.As(err, &se) && se.Status == http.StatusServiceUnavailable {
			return Config{}, errors.New("the platform cannot render the ingress configuration right now; this passes, try again later (retryable)")
		}
		if errors.Is(err, client.ErrTooLarge) {
			return Config{}, errors.New("the platform's ingress configuration is larger than this agent will read")
		}
		return Config{}, fmt.Errorf("could not read the ingress configuration from the platform: %w", err)
	}
	return Check(w, p)
}

// text says whether s can be written as a file's content: valid UTF-8 with no NUL (the revision's framing uses NUL as its separator).
func text(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// Check validates the platform's answer against the signed settings and recomputes the revision, which must equal the signed one.
func Check(w Wire, p Params) (Config, error) {
	if len(w.Sites) > MaxSites || len(w.Certificates) > MaxCerts {
		return Config{}, fmt.Errorf("the ingress configuration names more than %d sites or %d certificates", MaxSites, MaxCerts)
	}
	c := Config{Revision: w.Revision, Problems: len(w.Problems)}
	seen := map[string]bool{}
	for i, s := range w.Sites {
		if !ValidSiteFile(s.File) {
			return Config{}, fmt.Errorf("site %d of the ingress configuration is not a site file the agent writes (00-nuit-cloud-default.conf or nuit-cloud-<name>.conf)", i+1)
		}
		if seen[s.File] {
			return Config{}, fmt.Errorf("the ingress configuration names %s twice", s.File)
		}
		seen[s.File] = true
		if len(s.Content) == 0 || len(s.Content) > MaxSiteBytes || !text(s.Content) {
			return Config{}, fmt.Errorf("the content of %s is empty, too long or not text", s.File)
		}
		c.Sites = append(c.Sites, Site{File: s.File, Content: s.Content})
	}
	dirs := map[string]bool{}
	for i, x := range w.Certificates {
		if !certDirOK(x.Dir, p.CertRoot) {
			return Config{}, fmt.Errorf("certificate %d of the ingress configuration is not in a plain directory below %s", i+1, p.CertRoot)
		}
		if dirs[x.Dir] {
			return Config{}, fmt.Errorf("the ingress configuration names the directory %s twice", x.Dir)
		}
		dirs[x.Dir] = true
		if len(x.FullchainPEM) > MaxPEMBytes || len(x.PrivkeyPEM) > MaxPEMBytes || !text(x.FullchainPEM) || !text(x.PrivkeyPEM) ||
			!certPEM.MatchString(x.FullchainPEM) || !keyPEM.MatchString(x.PrivkeyPEM) {
			return Config{}, fmt.Errorf("the certificate or key for %s is not a usable PEM", x.Dir)
		}
		c.Certificates = append(c.Certificates, Certificate{Dir: x.Dir, FullchainPEM: x.FullchainPEM, PrivkeyPEM: x.PrivkeyPEM})
	}
	got := Revision(c.Sites, c.Certificates)
	if got != p.Revision {
		return Config{}, fmt.Errorf("the platform's ingress configuration is revision %s, not the signed %s (it changed after the task was signed, or it was altered); nothing was changed", short(got), short(p.Revision))
	}
	if w.Revision != got {
		return Config{}, fmt.Errorf("the platform's ingress configuration says it is revision %q but its content is %s; nothing was changed", short(w.Revision), short(got))
	}
	sort.Slice(c.Sites, func(i, j int) bool { return c.Sites[i].File < c.Sites[j].File })
	sort.Slice(c.Certificates, func(i, j int) bool { return c.Certificates[i].Dir < c.Certificates[j].Dir })
	return c, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// certDirOK: absolute, already clean, strictly below root, each part below it a plain name (or `*.` and one), within the length limit.
func certDirOK(dir, root string) bool {
	if len(dir) > maxPathLen || filepath.Clean(dir) != dir || !within(dir, root) {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(dir, root+"/"), "/") {
		if !certPartRe.MatchString(part) || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Provisioner prepares the profile's scripts: for an apply it fetches, checks and stages the content.
type Provisioner struct {
	Source Source
	// StageRoot is where a stage directory is made; defaults to DefaultStageRoot (created 0700 if missing).
	StageRoot string
}

// Prepare returns the environment for the profile's scripts, a function that removes every certificate and key from a text, and a cleanup
// that removes the stage (call it once the scripts are done). A check fetches nothing: it compares what was last applied with the signed
// revision. An apply fetches, checks and stages; any refusal happens before anything on the machine is touched.
func (p Provisioner) Prepare(ctx context.Context, params Params, apply bool) (env []string, scrub func(string) string, cleanup func(), err error) {
	scrub, cleanup = func(s string) string { return s }, func() {}
	env = []string{
		"NCS_INGRESS_REVISION=" + params.Revision,
		"NCS_INGRESS_CONF_DIR=" + params.ConfDir,
		"NCS_INGRESS_CERT_ROOT=" + params.CertRoot,
		"NCS_INGRESS_CONTAINER=" + params.Container,
	}
	if !apply {
		return env, scrub, cleanup, nil
	}
	if p.Source == nil {
		return nil, scrub, cleanup, errors.New("this agent cannot read the platform's ingress configuration")
	}
	cfg, err := Fetch(ctx, p.Source, params)
	if err != nil {
		return nil, scrub, cleanup, err
	}
	scrub = scrubber(cfg)
	dir, err := stage(p.StageRoot, cfg)
	if err != nil {
		return nil, scrub, cleanup, fmt.Errorf("could not stage the ingress configuration: %s", scrub(err.Error()))
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	env = append(env, "NCS_INGRESS_STAGE="+dir, "NCS_INGRESS_PROBLEMS="+strconv.Itoa(cfg.Problems))
	return env, scrub, cleanup, nil
}

// stage writes the content into a new root-only directory:
//
//	revision                 the (signed, recomputed) revision
//	sites.list               one site file name per line
//	sites/<file>             each site file's content
//	certs.list               "<n> <dir>" per line, n from 1
//	certs/<n>/fullchain.pem  and privkey.pem
//
// Every file is 0600 and every directory 0700; the content is written exactly, byte for byte.
func stage(root string, c Config) (dir string, err error) {
	if root == "" {
		root = DefaultStageRoot
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err = os.MkdirTemp(root, "ingress-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	put := func(rel, data string) error {
		return os.WriteFile(filepath.Join(dir, rel), []byte(data), 0o600)
	}
	if err := os.Mkdir(filepath.Join(dir, "sites"), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Join(dir, "certs"), 0o700); err != nil {
		return "", err
	}
	var sites, certs strings.Builder
	for _, s := range c.Sites {
		if err := put(filepath.Join("sites", s.File), s.Content); err != nil {
			return "", err
		}
		sites.WriteString(s.File + "\n")
	}
	for i, x := range c.Certificates {
		n := strconv.Itoa(i + 1)
		if err := os.Mkdir(filepath.Join(dir, "certs", n), 0o700); err != nil {
			return "", err
		}
		if err := put(filepath.Join("certs", n, "fullchain.pem"), x.FullchainPEM); err != nil {
			return "", err
		}
		if err := put(filepath.Join("certs", n, "privkey.pem"), x.PrivkeyPEM); err != nil {
			return "", err
		}
		certs.WriteString(n + " " + x.Dir + "\n")
	}
	if err := put("sites.list", sites.String()); err != nil {
		return "", err
	}
	if err := put("certs.list", certs.String()); err != nil {
		return "", err
	}
	if err := put("revision", c.Revision+"\n"); err != nil {
		return "", err
	}
	return dir, nil
}

// scrubber replaces every certificate and key (whole, and each line of their bodies) found in a text.
func scrubber(c Config) func(string) string {
	var secrets []string
	add := func(pem string) {
		secrets = append(secrets, pem, strings.TrimSpace(pem))
		for _, line := range strings.Split(pem, "\n") {
			line = strings.TrimSpace(line)
			if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
				secrets = append(secrets, line)
			}
		}
	}
	for _, x := range c.Certificates {
		add(x.PrivkeyPEM)
		add(x.FullchainPEM)
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

// Settings is what the profile records beside the revision when an apply succeeds (StateDir/settings), so the observation knows which
// directory and container to look at without a task.
type Settings struct {
	ConfDir   string
	CertRoot  string
	Container string
}

// ParseSettings reads StateDir/settings (`conf-dir=`, `cert-root=`, `container=` lines), checking each value as strictly as a task's.
func ParseSettings(text string) (Settings, bool) {
	var s Settings
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Settings{}, false
		}
		switch k {
		case "conf-dir":
			s.ConfDir = v
		case "cert-root":
			s.CertRoot = v
		case "container":
			s.Container = v
		default:
			return Settings{}, false
		}
	}
	if !ValidPath(s.ConfDir) || !ValidPath(s.CertRoot) || !ValidContainer(s.Container) {
		return Settings{}, false
	}
	return s, true
}
