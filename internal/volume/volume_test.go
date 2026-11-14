package volume

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
)

const (
	tenant   = "t-6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7"
	vol      = tenant + "/pgdata.img"
	transfer = "0b9a7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	credTest = "nca_FIXTURE-VOLUME-TEST"
)

// fakePlatform is the platform's half of a transfer: it keeps the chunks it is sent and gives them back.
type fakePlatform struct {
	t  *testing.T
	mu sync.Mutex
	// chunks by transfer, then index
	chunks map[string]map[int][]byte
	puts   int
	// failPutOnce answers the first put of this index with a 503 (a passing failure the agent must try again)
	failPutOnce map[int]bool
	// refusePut answers every put with this status and error text
	refusePut int
	refuseMsg string
	// corruptGet gives this index back with a hash that does not match it
	corruptGet int
	headers    http.Header
}

func newPlatform(t *testing.T) (*fakePlatform, *client.Client) {
	t.Helper()
	p := &fakePlatform{t: t, chunks: map[string]map[int][]byte{}, failPutOnce: map[int]bool{}, corruptGet: -1}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, "9.9.9", true)
	if err != nil {
		t.Fatal(err)
	}
	c.Credential = credTest
	return p, c
}

func (p *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.headers = r.Header.Clone()
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+credTest || r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"who are you"}`))
		return
	}
	var body struct {
		TransferID string `json:"transferId"`
		Index      int    `json:"index"`
		Data       string `json:"data"`
		SHA256     string `json:"sha256"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	switch r.URL.Path {
	case PutPath:
		p.puts++
		if p.refusePut != 0 {
			w.WriteHeader(p.refusePut)
			_, _ = w.Write([]byte(`{"error":"` + p.refuseMsg + `"}`))
			return
		}
		if p.failPutOnce[body.Index] {
			delete(p.failPutOnce, body.Index)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		data, err := base64.StdEncoding.DecodeString(body.Data)
		sum := sha256.Sum256(data)
		if err != nil || hex.EncodeToString(sum[:]) != body.SHA256 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"the chunk does not match its checksum"}`))
			return
		}
		if p.chunks[body.TransferID] == nil {
			p.chunks[body.TransferID] = map[int][]byte{}
		}
		p.chunks[body.TransferID][body.Index] = data
		w.WriteHeader(http.StatusNoContent)
	case GetPath:
		data, ok := p.chunks[body.TransferID][body.Index]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such chunk"}`))
			return
		}
		sum := sha256.Sum256(data)
		if body.Index == p.corruptGet {
			sum[0] ^= 0xff
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"data": base64.StdEncoding.EncodeToString(data), "sha256": hex.EncodeToString(sum[:])})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func noWait(context.Context, int) error { return nil }

// sparseImage makes a 64 MiB image that is mostly holes, with incompressible data in two places (so the transfer takes several chunks)
// and a little text near the end. It returns its bytes' SHA-256.
func sparseImage(t *testing.T, root string) (string, []byte) {
	t.Helper()
	dir := filepath.Join(root, tenant)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "pgdata.img"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 64 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, 600<<10)
	_, _ = rand.Read(noise)
	_, _ = f.WriteAt(noise[:400<<10], 1<<20)
	_, _ = f.WriteAt(noise[400<<10:], 40<<20+123)
	_, _ = f.WriteAt([]byte("ext4 superblock stand-in"), size-8192)
	all, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(all)
	return hex.EncodeToString(sum[:]), all
}

// allocated is how many bytes of the file take space on disk, and whether the filesystem says.
func allocated(t *testing.T, path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int64(st.Blocks) * 512, true
}

// holdLock takes the lock a running pod's VMM holds on an image.
func holdLock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestTheParamsAreExactlyTheContractAndAVolumeIsAlwaysBelowTheRoot(t *testing.T) {
	good := map[string]any{"transferId": transfer, "volume": vol}
	if p, err := ParseExport(good); err != nil || p.Volume != vol || p.TransferID != transfer {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := ParseImport(map[string]any{"transferId": transfer, "volume": vol, "size": float64(1 << 40), "sha256": strings.Repeat("a", 64), "chunks": float64(3)}); err != nil || p.Size != 1<<40 || p.Chunks != 3 {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := ParseRemove(map[string]any{"volume": vol}); err != nil || p.SHA256 != "" {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := ParseRemove(map[string]any{"volume": vol, "sha256": strings.Repeat("0", 64)}); err != nil || p.SHA256 != strings.Repeat("0", 64) {
		t.Fatalf("%+v %v", p, err)
	}
	hostileVolumes := []any{
		"../../etc/shadow", tenant + "/../x.img", tenant + "/..img", tenant + "/a..b.img", "/" + vol, vol + "/", tenant + "/x", tenant + "/.x.img",
		tenant + "/x.img\n", tenant + "/x y.img", tenant + "/x;rm.img", "t-6F1C2A3B-4D5E-4F60-8A71-92B3C4D5E6F7/x.img", "t-1/x.img",
		tenant + "/" + strings.Repeat("a", 64) + ".img", tenant + "/sub/x.img", tenant + "//x.img", nil, 7, "",
	}
	for _, v := range hostileVolumes {
		if _, err := ParseExport(map[string]any{"transferId": transfer, "volume": v}); err == nil {
			t.Errorf("export accepted volume %q", v)
		}
		if _, err := ParseRemove(map[string]any{"volume": v}); err == nil {
			t.Errorf("remove accepted volume %q", v)
		}
	}
	for _, bad := range []map[string]any{
		{"transferId": transfer, "volume": vol, "extra": 1},
		{"transferId": strings.ToUpper(transfer), "volume": vol},
		{"transferId": "x", "volume": vol},
		{"volume": vol},
	} {
		if _, err := ParseExport(bad); err == nil {
			t.Errorf("export accepted %v", bad)
		}
	}
	importGood := func() map[string]any {
		return map[string]any{"transferId": transfer, "volume": vol, "size": float64(1024), "sha256": strings.Repeat("a", 64), "chunks": float64(1)}
	}
	for k, v := range map[string]any{"size": float64(0), "chunks": float64(0), "sha256": strings.Repeat("A", 64), "volume": "../x.img", "unknown": true} {
		m := importGood()
		m[k] = v
		if _, err := ParseImport(m); err == nil {
			t.Errorf("import accepted %s=%v", k, v)
		}
	}
	for _, size := range []any{float64(1<<40 + 1), 1.5, "1024", float64(-1)} {
		m := importGood()
		m["size"] = size
		if _, err := ParseImport(m); err == nil {
			t.Errorf("import accepted size %v", size)
		}
	}
	m := importGood()
	delete(m, "chunks")
	if _, err := ParseImport(m); err == nil {
		t.Error("import accepted no chunk count")
	}
	if _, err := ParseRemove(map[string]any{"volume": vol, "sha256": "abc"}); err == nil {
		t.Error("remove accepted a short sha256")
	}
	if _, err := ParseRemove(map[string]any{"volume": vol, "transferId": transfer}); err == nil {
		t.Error("remove accepted a key it does not carry")
	}
}

func TestExportSendsTheImageGzippedInChunksWithTheirHashesAndAnImportWritesTheSameSparseFile(t *testing.T) {
	src := t.TempDir()
	sum, raw := sparseImage(t, src)
	plat, c := newPlatform(t)
	plat.failPutOnce[1] = true // the platform fails once: the agent tries that chunk again
	m := Mover{Root: src, Platform: c, Wait: noWait}

	res, err := m.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != int64(len(raw)) || res.SHA256 != sum || res.Chunks < 3 || res.Chunks != len(plat.chunks[transfer]) {
		t.Fatalf("result %+v (chunks held %d)", res, len(plat.chunks[transfer]))
	}
	if plat.puts != res.Chunks+1 || plat.headers.Get("X-Agent-Version") != "9.9.9" {
		t.Errorf("puts %d for %d chunks, headers %v", plat.puts, res.Chunks, plat.headers)
	}
	var joined []byte
	for i := 0; i < res.Chunks; i++ {
		ch, ok := plat.chunks[transfer][i]
		if !ok || len(ch) == 0 || len(ch) > ChunkBytes || (i < res.Chunks-1 && len(ch) != ChunkBytes) {
			t.Fatalf("chunk %d: %d bytes", i, len(ch))
		}
		joined = append(joined, ch...)
	}
	if int64(len(joined)) != res.CompressedBytes {
		t.Errorf("compressed %d, chunks hold %d", res.CompressedBytes, len(joined))
	}
	zr, err := gzip.NewReader(bytes.NewReader(joined))
	if err != nil {
		t.Fatal(err)
	}
	back, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(back, raw) {
		t.Fatalf("the chunks are not the image gzipped: %v", err)
	}

	dst := t.TempDir()
	in := Mover{Root: dst, Platform: c, Wait: noWait}
	got, err := in.Import(context.Background(), ImportParams{TransferID: transfer, Volume: vol, Size: res.Size, SHA256: res.SHA256, Chunks: res.Chunks})
	if err != nil {
		t.Fatal(err)
	}
	if got.Already || got.Size != res.Size || got.SHA256 != sum {
		t.Fatalf("%+v", got)
	}
	file := filepath.Join(dst, tenant, "pgdata.img")
	written, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(written, raw) {
		t.Fatalf("the imported image differs from the exported one: %v", err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("image mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dst, tenant)); fi.Mode().Perm() != 0o700 {
		t.Errorf("tenant directory mode %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Join(dst, tenant))
	if len(entries) != 1 {
		t.Errorf("the temporary file was left: %v", entries)
	}
	// still sparse, where the filesystem says (the source shows whether this one makes holes at all)
	if srcAlloc, ok := allocated(t, filepath.Join(src, tenant, "pgdata.img")); ok && srcAlloc < int64(len(raw))/4 {
		dstAlloc, _ := allocated(t, file)
		t.Logf("allocated: source %d, import %d, of %d bytes", srcAlloc, dstAlloc, len(raw))
		if dstAlloc > int64(len(raw))/4 {
			t.Errorf("the import is not sparse: %d bytes allocated for a %d byte image (the source has %d)", dstAlloc, len(raw), srcAlloc)
		}
	} else {
		t.Log("this filesystem does not report holes; sparseness not checked")
	}

	// tried again (the platform did not hear the first answer): the same copy is already here
	again, err := in.Import(context.Background(), ImportParams{TransferID: transfer, Volume: vol, Size: res.Size, SHA256: res.SHA256, Chunks: res.Chunks})
	if err != nil || !again.Already || again.SHA256 != sum {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestAnImportNeverOverwritesADifferentVolume(t *testing.T) {
	src := t.TempDir()
	sum, raw := sparseImage(t, src)
	_, c := newPlatform(t)
	res, err := Mover{Root: src, Platform: c, Wait: noWait}.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	file := filepath.Join(dst, tenant, "pgdata.img")
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	other := bytes.Repeat([]byte{1}, len(raw)) // the same size, other bytes
	if err := os.WriteFile(file, other, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Mover{Root: dst, Platform: c, Wait: noWait}.Import(context.Background(), ImportParams{TransferID: transfer, Volume: vol, Size: res.Size, SHA256: sum, Chunks: res.Chunks})
	if err == nil || !strings.Contains(err.Error(), "a different volume "+vol+" is already on this machine") {
		t.Fatalf("%v", err)
	}
	if now, _ := os.ReadFile(file); !bytes.Equal(now, other) {
		t.Error("the volume already there was changed")
	}
	if entries, _ := os.ReadDir(filepath.Dir(file)); len(entries) != 1 {
		t.Errorf("something was left beside it: %v", entries)
	}
	// a different size is different without reading it
	if err := os.WriteFile(file, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Mover{Root: dst, Platform: c, Wait: noWait}).Import(context.Background(), ImportParams{TransferID: transfer, Volume: vol, Size: res.Size, SHA256: sum, Chunks: res.Chunks}); err == nil {
		t.Error("an image of another size was taken for this one")
	}
}

func TestAChunkThatDoesNotMatchItsHashFailsTheImportAndLeavesNothing(t *testing.T) {
	src := t.TempDir()
	sum, _ := sparseImage(t, src)
	plat, c := newPlatform(t)
	res, err := Mover{Root: src, Platform: c, Wait: noWait}.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	plat.corruptGet = 1
	dst := t.TempDir()
	_, err = Mover{Root: dst, Platform: c, Wait: noWait}.Import(context.Background(), ImportParams{TransferID: transfer, Volume: vol, Size: res.Size, SHA256: sum, Chunks: res.Chunks})
	if err == nil || !strings.Contains(err.Error(), "chunk 1 from the platform does not match its checksum") {
		t.Fatalf("%v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dst, tenant)); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestAnImportWhoseBytesAreNotTheSignedOnesIsNotPutInPlace(t *testing.T) {
	src := t.TempDir()
	sum, _ := sparseImage(t, src)
	_, c := newPlatform(t)
	res, err := Mover{Root: src, Platform: c, Wait: noWait}.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]ImportParams{
		"another checksum":     {TransferID: transfer, Volume: vol, Size: res.Size, SHA256: strings.Repeat("0", 64), Chunks: res.Chunks},
		"a smaller size":       {TransferID: transfer, Volume: vol, Size: res.Size - 4096, SHA256: sum, Chunks: res.Chunks},
		"a larger size":        {TransferID: transfer, Volume: vol, Size: res.Size + 4096, SHA256: sum, Chunks: res.Chunks},
		"fewer chunks":         {TransferID: transfer, Volume: vol, Size: res.Size, SHA256: sum, Chunks: res.Chunks - 1},
		"chunks it never sent": {TransferID: transfer, Volume: vol, Size: res.Size, SHA256: sum, Chunks: res.Chunks + 1},
	}
	for name, p := range cases {
		dst := t.TempDir()
		if _, err := (Mover{Root: dst, Platform: c, Wait: noWait}).Import(context.Background(), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if entries, _ := os.ReadDir(filepath.Join(dst, tenant)); len(entries) != 0 {
			t.Errorf("%s: left %v", name, entries)
		}
	}
}

func TestALockedVolumeIsNeitherExportedNorRemoved(t *testing.T) {
	root := t.TempDir()
	sparseImage(t, root)
	file := filepath.Join(root, tenant, "pgdata.img")
	plat, c := newPlatform(t)
	m := Mover{Root: root, Platform: c, Wait: noWait}
	lock := holdLock(t, file)
	if _, err := m.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol}); err == nil || !strings.Contains(err.Error(), vol+" is in use by a running pod") {
		t.Errorf("export: %v", err)
	}
	if plat.puts != 0 {
		t.Errorf("%d chunks were sent of a volume in use", plat.puts)
	}
	if _, err := m.Remove(context.Background(), RemoveParams{Volume: vol}); err == nil || !strings.Contains(err.Error(), "in use by a running pod") {
		t.Errorf("remove: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("a volume in use was removed: %v", err)
	}
	// the pod stops: now it can go
	lock.Close()
	if res, err := m.Remove(context.Background(), RemoveParams{Volume: vol}); err != nil || !res.Removed {
		t.Errorf("%+v %v", res, err)
	}
}

func TestRemoveDeletesOnlyTheCopyItIsToldAndAMissingVolumeIsNotAFailure(t *testing.T) {
	root := t.TempDir()
	sum, _ := sparseImage(t, root)
	file := filepath.Join(root, tenant, "pgdata.img")
	m := Mover{Root: root}
	if _, err := m.Remove(context.Background(), RemoveParams{Volume: vol, SHA256: strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "not the copy the platform expects") {
		t.Errorf("%v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("a volume that is not the expected copy was removed")
	}
	if res, err := m.Remove(context.Background(), RemoveParams{Volume: vol, SHA256: sum}); err != nil || !res.Removed {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the volume is still there")
	}
	if res, err := m.Remove(context.Background(), RemoveParams{Volume: vol}); err != nil || res.Removed {
		t.Errorf("again: %+v %v", res, err)
	}
	if res, err := (Mover{Root: t.TempDir()}).Remove(context.Background(), RemoveParams{Volume: vol}); err != nil || res.Removed {
		t.Errorf("no tenant directory: %+v %v", res, err)
	}
}

func TestExportOfAMissingVolumeFailsAndARefusalStopsTheTransferWithThePlatformsWords(t *testing.T) {
	root := t.TempDir()
	plat, c := newPlatform(t)
	m := Mover{Root: root, Platform: c, Wait: noWait}
	if _, err := m.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol}); err == nil || err.Error() != "no volume "+vol+" on this machine" {
		t.Errorf("%v", err)
	}
	sparseImage(t, root)
	plat.refusePut, plat.refuseMsg = http.StatusConflict, "the transfer is closed"
	_, err := m.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol})
	if err == nil || !strings.Contains(err.Error(), "the transfer is closed") || plat.puts != 1 {
		t.Errorf("%v after %d puts", err, plat.puts)
	}
}

func TestAPlatformThatKeepsFailingIsTriedFourTimesForAChunk(t *testing.T) {
	root := t.TempDir()
	sparseImage(t, root)
	plat, c := newPlatform(t)
	plat.refusePut, plat.refuseMsg = http.StatusBadGateway, "down"
	waits := 0
	m := Mover{Root: root, Platform: c, Wait: func(context.Context, int) error { waits++; return nil }}
	if _, err := m.Export(context.Background(), ExportParams{TransferID: transfer, Volume: vol}); err == nil {
		t.Fatal("accepted")
	}
	if plat.puts != 4 || waits != 3 {
		t.Errorf("%d puts, %d waits", plat.puts, waits)
	}
}

func TestAVolumeBehindASymbolicLinkIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sparseImage(t, outside)
	if err := os.Symlink(filepath.Join(outside, tenant), filepath.Join(root, tenant)); err != nil {
		t.Fatal(err)
	}
	m := Mover{Root: root}
	if _, err := m.Remove(context.Background(), RemoveParams{Volume: vol}); err == nil {
		t.Error("a volume through a linked tenant directory was removed")
	}
	if _, err := os.Stat(filepath.Join(outside, tenant, "pgdata.img")); err != nil {
		t.Error("the file outside the root is gone")
	}
	root2 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root2, tenant), 0o700)
	if err := os.Symlink(filepath.Join(outside, tenant, "pgdata.img"), filepath.Join(root2, tenant, "pgdata.img")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Mover{Root: root2}).Remove(context.Background(), RemoveParams{Volume: vol}); err == nil {
		t.Error("a linked image was removed")
	}
}
