// Package runtimeinstall fetches what the Nuit Container Service runtime is installed from, when the platform asks for a
// profile-apply that includes it: the oci-saas bundle and a pinned containerd release. Both come from the platform's own
// address and are checked against the SHA-256 in the signed task before anything uses them; the root-key fingerprint the
// bundle's keys must chain to is carried the same way. The agent only stages the files and hands their paths to the profile's
// scripts (`oci_saas_runtime` in internal/profiles/bootstrap): installing is the scripts' job, one definition whether a person or the
// agent runs it.
package runtimeinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Doer is what sends a download request: the agent's client (which knows the platform's fallback addresses) or a plain http.Client.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Files the platform serves, by name; the agent asks for these and nothing else.
const (
	BundleFile     = "oci-saas-bundle.tar.gz"
	ContainerdFile = "containerd.tar.gz"
	CrictlFile     = "crictl.tar.gz"
)

const (
	maxBundleBytes     = 512 << 20
	maxContainerdBytes = 128 << 20
	maxCrictlBytes     = 64 << 20
)

var (
	hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	version   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// Params are what a signed profile-apply carries for the runtime.
type Params struct {
	// Arch is the architecture the pins are for (amd64 or arm64). Empty means a platform that predates architectures: the unsuffixed files, amd64.
	Arch              string
	BundleVersion     string
	BundleSHA256      string
	ContainerdVersion string
	ContainerdSHA256  string
	CrictlVersion     string
	CrictlSHA256      string
	RootFingerprint   string
}

// ParseParams reads and validates the `runtime` parameter of a profile task.
func ParseParams(raw map[string]any) (Params, error) {
	str := func(k string) string { s, _ := raw[k].(string); return s }
	p := Params{
		Arch: str("arch"), BundleVersion: str("bundleVersion"), BundleSHA256: str("bundleSha256"),
		ContainerdVersion: str("containerdVersion"), ContainerdSHA256: str("containerdSha256"),
		CrictlVersion: str("crictlVersion"), CrictlSHA256: str("crictlSha256"),
		RootFingerprint: str("rootFingerprint"),
	}
	if p.Arch != "" && p.Arch != "amd64" && p.Arch != "arm64" {
		return Params{}, fmt.Errorf("the runtime parameters name an architecture this agent does not know (%q)", p.Arch)
	}
	for _, v := range []string{p.BundleVersion, p.ContainerdVersion, p.CrictlVersion} {
		if !version.MatchString(v) {
			return Params{}, fmt.Errorf("the runtime parameters carry no usable version (%q)", v)
		}
	}
	for name, v := range map[string]string{"bundle": p.BundleSHA256, "containerd": p.ContainerdSHA256, "crictl": p.CrictlSHA256, "root fingerprint": p.RootFingerprint} {
		if !hexSHA256.MatchString(v) {
			return Params{}, fmt.Errorf("the %s checksum is not a SHA-256", name)
		}
	}
	return p, nil
}

// Stager downloads the runtime's files into a directory under the agent's state.
type Stager struct {
	BaseURL string
	HTTP    Doer
	// Dir is where staging directories are made (the agent's state directory: /tmp may be a small tmpfs).
	Dir string
}

// Stage downloads and checks both files and returns the environment the profile scripts read, plus a function that removes
// the files. Nothing is returned for the scripts to use unless both checksums matched.
func (s Stager) Stage(ctx context.Context, p Params) (env []string, cleanup func(), err error) {
	// The files are built for one architecture: never stage another's on this machine, whatever the checksums say.
	if p.Arch != "" && p.Arch != hostArch() {
		return nil, nil, fmt.Errorf("these runtime files are for %s and this machine is %s", p.Arch, hostArch())
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(s.Dir, "runtime-")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	bundle, err := s.fetch(ctx, named(BundleFile, p.Arch), filepath.Join(dir, BundleFile), p.BundleSHA256, maxBundleBytes)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	containerd, err := s.fetch(ctx, named(ContainerdFile, p.Arch), filepath.Join(dir, ContainerdFile), p.ContainerdSHA256, maxContainerdBytes)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	crictl, err := s.fetch(ctx, named(CrictlFile, p.Arch), filepath.Join(dir, CrictlFile), p.CrictlSHA256, maxCrictlBytes)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return []string{
		"NCS_CRICTL_TARBALL=" + crictl,
		"NCS_RUNTIME_BUNDLE=" + bundle,
		"NCS_RUNTIME_BUNDLE_SHA256=" + p.BundleSHA256,
		"NCS_RUNTIME_ROOT_FINGERPRINT=" + p.RootFingerprint,
		"NCS_CONTAINERD_TARBALL=" + containerd,
	}, cleanup, nil
}

// hostArch is this machine's architecture as the platform names it. A variable so tests can stand in for another machine.
var hostArch = func() string { return runtime.GOARCH }

// named is the name the platform serves a file under for an architecture: `containerd.tar.gz` becomes `containerd-arm64.tar.gz`. With no
// architecture it is the old, unsuffixed name.
func named(file, arch string) string {
	if arch == "" {
		return file
	}
	return strings.TrimSuffix(file, ".tar.gz") + "-" + arch + ".tar.gz"
}

// Fetch downloads one of the files the platform serves under /api/agent/download/ into dst (which must not exist), refusing more than
// limit bytes, and keeps it only if its SHA-256 is want. The same code path as the runtime's own files: the agent's client, so the
// platform's name and its fallback addresses are used and TLS is checked against the name. Other packages (the control-plane bundle)
// use it so there is one download path.
func (s Stager) Fetch(ctx context.Context, name, dst, want string, limit int64) (string, error) {
	return s.fetch(ctx, name, dst, want, limit)
}

func (s Stager) fetch(ctx context.Context, name, dst, want string, limit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.BaseURL, "/")+"/api/agent/download/"+name, nil)
	if err != nil {
		return "", err
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Minute}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot download %s: %w", name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the platform answered %d when asked for %s", res.StatusCode, name)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("the download of %s broke off: %w", name, err)
	}
	if n > limit {
		return "", fmt.Errorf("%s is far larger than expected; refusing it", name)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", errors.New(name + " does not match the checksum in the signed task; nothing was installed")
	}
	return dst, nil
}
