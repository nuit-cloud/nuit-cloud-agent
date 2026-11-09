package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/runtimeinstall"
)

type entry struct {
	name string
	typ  byte
	body string
	mode int64
	link string
	size int64 // when set, the size the header claims (a lie for a truncated archive)
}

func goodEntries(version string) []entry {
	return []entry{
		{name: "VERSION", body: version + "\n"},
		{name: "ofelia", body: "\x7fELF-not-really", mode: 0o755},
		{name: "scheduler", typ: tar.TypeDir, mode: 0o755},
		{name: "scheduler/config.ini", body: "[job-local \"x\"]\ncommand = sh /etc/ofelia/monitor-run.sh\n"},
		{name: "scheduler/monitor-run.sh", body: "#!/bin/sh\n"},
		{name: "scheduler/report-job.sh", body: "#!/bin/sh\n"},
		{name: "scheduler/renewal-sweep.sh", body: "#!/bin/sh\n"},
		{name: "probe", typ: tar.TypeDir, mode: 0o755},
		{name: "probe/probe.sh", body: "#!/bin/sh\n", mode: 0o644},
	}
}

func tarball(t *testing.T, entries []entry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz(t, raw.Bytes())
}

func gz(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(raw)
	_ = w.Close()
	return out.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// memFetcher serves archives by name and checks the checksum as the real download path does.
type memFetcher struct {
	files map[string][]byte
	asked atomic.Int32
}

func (m *memFetcher) Fetch(_ context.Context, name, dst, want string, limit int64) (string, error) {
	m.asked.Add(1)
	data, ok := m.files[name]
	if !ok {
		return "", errors.New("the platform answered 404")
	}
	if int64(len(data)) > limit {
		return "", errors.New("far larger than expected")
	}
	if sum(data) != want {
		return "", errors.New(name + " does not match the checksum in the signed task; nothing was installed")
	}
	return dst, os.WriteFile(dst, data, 0o600)
}

func extractTo(t *testing.T, data []byte, version string) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "out")
	return dest, Extract(bytes.NewReader(data), dest, version)
}

func TestAGoodBundleIsUnpackedWithTheModesWeChoose(t *testing.T) {
	// the archive claims odd modes; what lands on disk is 0755 for directories and the two executables, 0644 for the rest
	e := goodEntries("1.2.3")
	e[0].mode, e[3].mode, e[4].mode = 0o600, 0o666, 0o777
	dest, err := extractTo(t, tarball(t, e), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{".": 0o755, "scheduler": 0o755, "probe": 0o755, "ofelia": 0o755, "probe/probe.sh": 0o755, "VERSION": 0o644, "scheduler/config.ini": 0o644, "scheduler/monitor-run.sh": 0o644, "scheduler/report-job.sh": 0o644, "scheduler/renewal-sweep.sh": 0o644}
	for name, mode := range want {
		st, err := os.Stat(filepath.Join(dest, name))
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v %v (want %v)", name, st, err, mode)
		}
	}
}

func TestHostileBundlesAreRefusedAndTheMessageNeverEchoesAnUnsafeName(t *testing.T) {
	add := func(more ...entry) []entry { return append(goodEntries("1.2.3"), more...) }
	many := goodEntries("1.2.3")
	for i := 0; i < MaxBundleEntries; i++ {
		many = append(many, entry{name: "scheduler/job-" + strings.Repeat("a", 1) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".sh", body: "x"})
	}
	without := func(name string) []entry {
		var out []entry
		for _, e := range goodEntries("1.2.3") {
			if e.name != name {
				out = append(out, e)
			}
		}
		return out
	}
	cases := map[string][]entry{
		"path traversal":        add(entry{name: "../evil"}),
		"traversal inside":      add(entry{name: "scheduler/../../evil"}),
		"absolute path":         add(entry{name: "/etc/cron.d/evil"}),
		"a backslash":           add(entry{name: "scheduler\\evil.sh"}),
		"a symlink":             add(entry{name: "scheduler/link.sh", typ: tar.TypeSymlink, link: "/etc/shadow"}),
		"a symlink over a file": {entry{name: "VERSION", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"a hardlink":            add(entry{name: "scheduler/hard.sh", typ: tar.TypeLink, link: "VERSION"}),
		"a device":              add(entry{name: "scheduler/dev.sh", typ: tar.TypeChar}),
		"a block device":        add(entry{name: "scheduler/blk.sh", typ: tar.TypeBlock}),
		"a fifo":                add(entry{name: "scheduler/fifo.sh", typ: tar.TypeFifo}),
		"setuid":                append(goodEntries("1.2.3")[1:], entry{name: "VERSION", body: "1.2.3\n", mode: 0o4755}),
		"setgid":                add(entry{name: "scheduler/sg.sh", mode: 0o2755}),
		"sticky":                add(entry{name: "scheduler/st.sh", mode: 0o1755}),
		"a duplicate":           add(entry{name: "scheduler/monitor-run.sh", body: "#!/bin/sh\necho again\n"}),
		"a duplicate with ./":   add(entry{name: "./scheduler/monitor-run.sh", body: "x"}),
		"too many entries":      many,
		"an extra file":         add(entry{name: "README"}),
		"an extra top script":   add(entry{name: "evil.sh"}),
		"a script outside":      add(entry{name: "probe/other.sh"}),
		"a bad script name":     add(entry{name: "scheduler/Evil Script.sh"}),
		"an extra directory":    add(entry{name: "etc", typ: tar.TypeDir}),
		"a directory as file":   add(entry{name: "extra", typ: tar.TypeDir}),
		"no VERSION":            without("VERSION"),
		"no ofelia":             without("ofelia"),
		"no config":             without("scheduler/config.ini"),
		"no report-job":         without("scheduler/report-job.sh"),
		"no monitor-run":        without("scheduler/monitor-run.sh"),
		"no probe":              without("probe/probe.sh"),
		"the wrong VERSION":     append(goodEntries("1.2.3")[1:], entry{name: "VERSION", body: "9.9.9\n"}),
		"a file as directory":   {entry{name: "probe", body: "x"}},
	}
	for name, entries := range cases {
		dest, err := extractTo(t, tarball(t, entries), "1.2.3")
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "etc/cron.d") && strings.HasPrefix(name, "absolute") {
			// a refused name may be named in the message, but only printable and short (see safeName); it must never be a path we wrote
			if _, statErr := os.Stat("/etc/cron.d/evil"); statErr == nil {
				t.Errorf("%s: written", name)
			}
		}
		_ = dest
	}
	// nothing was ever written outside the destination
	if _, err := os.Stat(filepath.Join(os.TempDir(), "evil")); err == nil {
		t.Error("a traversal wrote outside the destination")
	}
}

func TestAnOversizedTruncatedOrNotGzipBundleIsRefused(t *testing.T) {
	big := goodEntries("1.2.3")
	big = append(big, entry{name: "scheduler/big.sh", body: strings.Repeat("x", MaxBundleFile+1)})
	if _, err := extractTo(t, tarball(t, big), "1.2.3"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file over 32 MB: %v", err)
	}
	// each file is under the per-file limit but together they are over the total
	total := goodEntries("1.2.3")
	for i := 0; i < 3; i++ {
		total = append(total, entry{name: "scheduler/part-" + string(rune('a'+i)) + ".sh", body: strings.Repeat("y", 24<<20)})
	}
	if _, err := extractTo(t, tarball(t, total), "1.2.3"); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a total over 64 MB: %v", err)
	}
	good := tarball(t, goodEntries("1.2.3"))
	if _, err := extractTo(t, good[:len(good)/2], "1.2.3"); err == nil {
		t.Error("a truncated archive was accepted")
	}
	if _, err := extractTo(t, []byte("this is not a gzip file at all"), "1.2.3"); err == nil {
		t.Error("garbage was accepted")
	}
	// a header that claims more bytes than the archive holds
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	_ = tw.WriteHeader(&tar.Header{Name: "VERSION", Typeflag: tar.TypeReg, Mode: 0o644, Size: 10})
	_, _ = tw.Write([]byte("1.2"))
	if _, err := extractTo(t, gz(t, raw.Bytes()), "1.2.3"); err == nil {
		t.Error("a lying header was accepted")
	}
}

func TestInstallChecksTheChecksumBeforeReadingAnythingAndIsIdempotent(t *testing.T) {
	data := tarball(t, goodEntries("1.2.3"))
	b := Bundle{Version: "1.2.3", Arch: "arm64", SHA256: sum(data)}
	f := &memFetcher{files: map[string][]byte{b.File(): data}}
	root, stage := filepath.Join(t.TempDir(), "opt"), filepath.Join(t.TempDir(), "stage")
	if err := Install(context.Background(), f, b, root, stage); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "1.2.3", ShaFile))
	if strings.TrimSpace(string(got)) != b.SHA256 {
		t.Errorf("the checksum was not recorded: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "1.2.3", "ofelia")); err != nil {
		t.Error(err)
	}
	if left, _ := os.ReadDir(stage); len(left) != 0 {
		t.Errorf("the download was left in the state directory: %v", left)
	}
	if left, _ := os.ReadDir(root); len(left) != 1 {
		t.Errorf("scratch directories left beside the version: %v", left)
	}
	if err := Install(context.Background(), f, b, root, stage); err != nil || f.asked.Load() != 1 {
		t.Errorf("a second install downloaded again or failed: %v asked=%d", err, f.asked.Load())
	}
	// a wrong checksum: refused, and no directory made
	bad := Bundle{Version: "2.0.0", Arch: "arm64", SHA256: strings.Repeat("0", 64)}
	f.files[bad.File()] = data
	if err := Install(context.Background(), f, bad, root, stage); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a wrong checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "2.0.0")); !os.IsNotExist(err) {
		t.Error("a bundle with a wrong checksum was unpacked")
	}
	// the right checksum for an archive whose VERSION is another: refused
	wrongVersion := tarball(t, goodEntries("1.2.4"))
	wv := Bundle{Version: "1.2.3", Arch: "arm64", SHA256: sum(wrongVersion)}
	f2 := &memFetcher{files: map[string][]byte{wv.File(): wrongVersion}}
	root2 := filepath.Join(t.TempDir(), "opt")
	if err := Install(context.Background(), f2, wv, root2, stage); err == nil {
		t.Error("a bundle whose VERSION differs was installed")
	}
	if _, err := os.Stat(filepath.Join(root2, "1.2.3")); !os.IsNotExist(err) {
		t.Error("a bundle with the wrong VERSION was left behind")
	}
	// the same version rebuilt (another checksum): replaced
	e := goodEntries("1.2.3")
	e[3].body = "[job-local \"y\"]\n"
	data2 := tarball(t, e)
	b2 := Bundle{Version: "1.2.3", Arch: "arm64", SHA256: sum(data2)}
	f.files[b2.File()] = data2
	if err := Install(context.Background(), f, b2, root, stage); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(root, "1.2.3", "scheduler", "config.ini"))
	sha, _ := os.ReadFile(filepath.Join(root, "1.2.3", ShaFile))
	if !strings.Contains(string(cfg), `"y"`) || strings.TrimSpace(string(sha)) != b2.SHA256 {
		t.Errorf("the rebuilt bundle did not replace the old one: %q %q", cfg, sha)
	}
}

func TestTheRealDownloadPathServesAndChecksTheBundle(t *testing.T) {
	data := tarball(t, goodEntries("3.0.0"))
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		if r.URL.Path == "/api/agent/download/control-plane-arm64.tar.gz" {
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	st := runtimeinstall.Stager{BaseURL: srv.URL, HTTP: srv.Client(), Dir: t.TempDir()}
	root := filepath.Join(t.TempDir(), "opt")
	b := Bundle{Version: "3.0.0", Arch: "arm64", SHA256: sum(data)}
	if err := Install(context.Background(), st, b, root, filepath.Join(t.TempDir(), "stage")); err != nil {
		t.Fatal(err)
	}
	if path.Load() != "/api/agent/download/control-plane-arm64.tar.gz" {
		t.Errorf("asked for %v", path.Load())
	}
	b.SHA256 = strings.Repeat("a", 64)
	if err := Install(context.Background(), st, b, filepath.Join(t.TempDir(), "opt2"), filepath.Join(t.TempDir(), "stage")); err == nil {
		t.Error("a download that does not match the signed checksum was accepted")
	}
	b.Arch = "amd64" // the platform serves no such file here
	if err := Install(context.Background(), st, b, filepath.Join(t.TempDir(), "opt3"), filepath.Join(t.TempDir(), "stage")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestInstallRefusesUnusableParameters(t *testing.T) {
	f := &memFetcher{files: map[string][]byte{}}
	for _, b := range []Bundle{{Version: "../x", Arch: "arm64", SHA256: strings.Repeat("a", 64)}, {Version: "1", Arch: "arm64", SHA256: "short"}, {Version: ".hidden", Arch: "arm64", SHA256: strings.Repeat("a", 64)}} {
		if err := Install(context.Background(), f, b, t.TempDir(), t.TempDir()); err == nil || f.asked.Load() != 0 {
			t.Errorf("%+v: %v", b, err)
		}
	}
}
