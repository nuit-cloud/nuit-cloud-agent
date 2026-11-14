package observe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/ingress"
)

// IngressFacts is the `ingress` observation.
type IngressFacts struct {
	// Revision is the revision last applied; null when none is recorded (or the record is not a revision).
	Revision *string `json:"revision"`
	// ContainerRunning says whether the nginx container the profile applies to is running.
	ContainerRunning bool `json:"containerRunning"`
	// Sites is how many of the platform's site files (00-nuit-cloud-default.conf, nuit-cloud-<name>.conf) are in the site directory.
	Sites int `json:"sites"`
	// NginxOK is whether `nginx -t` in the container accepts the configuration; null when it was not run (the container is not running,
	// or it did not answer in time).
	NginxOK *bool `json:"nginxOk"`
}

// nginxTestTimeout bounds `nginx -t` (it reads the configuration and the certificates, and changes nothing).
const nginxTestTimeout = 15 * time.Second

func readIngressState(name string) (string, error) {
	if name != "settings" && name != "revision" {
		return "", errors.New("not an ingress record")
	}
	b, err := os.ReadFile(filepath.Join(ingress.StateDir, name))
	return string(b), err
}

func listFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// ingress reads what the profile recorded and looks at the container and the site directory. A machine the profile was never applied to
// (no usable settings record) reports nothing at all. The directory and container come only from that record, checked again; nothing the
// platform sends is used here.
func (c Collector) ingress(ctx context.Context) (IngressFacts, bool) {
	if c.ReadIngressState == nil {
		return IngressFacts{}, false
	}
	text, err := c.ReadIngressState("settings")
	if err != nil {
		return IngressFacts{}, false
	}
	s, ok := ingress.ParseSettings(text)
	if !ok {
		return IngressFacts{}, false
	}
	var facts IngressFacts
	if rev, err := c.ReadIngressState("revision"); err == nil && ingress.ValidRevision(strings.TrimSpace(rev)) {
		r := strings.TrimSpace(rev)
		facts.Revision = &r
	}
	if c.ListDir != nil {
		if names, err := c.ListDir(s.ConfDir); err == nil {
			for _, n := range names {
				if ingress.ValidSiteFile(n) {
					facts.Sites++
				}
			}
		}
	}
	if c.Run == nil {
		return facts, true
	}
	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := c.Run(ictx, "docker", "inspect", "--format", "{{.State.Running}}", s.Container)
	cancel()
	facts.ContainerRunning = err == nil && strings.TrimSpace(out) == "true"
	if !facts.ContainerRunning {
		return facts, true
	}
	tctx, cancel := context.WithTimeout(ctx, nginxTestTimeout)
	defer cancel()
	_, err = c.Run(tctx, "docker", "exec", s.Container, "nginx", "-t")
	if tctx.Err() == nil {
		ok := err == nil
		facts.NginxOK = &ok
	}
	return facts, true
}

// version is a digest of everything reported, so an unchanged ingress sends nothing.
func (f IngressFacts) version() string {
	rev, nginx := "-", "-"
	if f.Revision != nil {
		rev = *f.Revision
	}
	if f.NginxOK != nil {
		nginx = fmt.Sprint(*f.NginxOK)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%t|%d|%s", rev, f.ContainerRunning, f.Sites, nginx)))
	return "i" + hex.EncodeToString(sum[:])[:12]
}
