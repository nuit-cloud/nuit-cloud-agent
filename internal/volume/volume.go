// Package volume copies a tenant's persistent volume from one Nuit Cloud Server to another through the platform (nuit-cloud/nuit-cloud#186).
//
// The Nuit Container Service keeps each persistent volume as a sparse ext4 image, `<root>/t-<tenant uuid>/<name>.img`, root-only. While a
// pod's microVM uses one, the runtime's VMM holds an exclusive flock on the file; a stopped pod releases it. Three signed tasks use this
// package, each naming one image by a path that is checked again here (shape, length, character set; nothing outside the volumes root):
//
//   - volume-export reads an image no pod holds, holding the lock itself for the whole read so no pod can start on it meanwhile, and sends
//     it to the platform gzipped in chunks of at most ChunkBytes (`POST /api/agent/volume-transfer/put`), with the image's SHA-256.
//   - volume-import fetches the chunks back (`POST /api/agent/volume-transfer/get`), checks each chunk's hash, writes the image sparsely
//     under a temporary name, checks its size and SHA-256 against the signed ones and only then puts it in place. It never overwrites an
//     image that is already there.
//   - volume-remove deletes an image no pod holds, under the lock, optionally only if it is the copy the platform expects.
//
// The agent never mounts or reads the filesystem inside an image: it moves bytes and checks their hash.
package volume

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
)

// DefaultRoot is where the Nuit Container Service keeps persistent volumes.
const DefaultRoot = "/var/lib/oci-saas/volumes"

// The platform's two routes for a transfer's chunks.
const (
	PutPath = "/api/agent/volume-transfer/put"
	GetPath = "/api/agent/volume-transfer/get"
)

// Limits (the platform keeps within the same ones).
const (
	// ChunkBytes is the most compressed bytes one chunk carries.
	ChunkBytes = 262144
	// MaxSize is the largest image an import accepts: 1 TiB.
	MaxSize = int64(1) << 40
	// MaxChunks is the most chunks an import accepts: enough for MaxSize even if gzip could not compress it at all.
	MaxChunks = 1 << 23
	// maxGetAnswer bounds the platform's answer to a get: one chunk in base64 and its hash, with room for the JSON around them.
	maxGetAnswer = 512 << 10
	// retries is how many times a chunk is tried again after a network error or a 5xx.
	retries = 3
	// block is the size of the runs of zeros the import leaves as holes.
	block = 4096
)

var (
	// volumeRe is `t-<tenant uuid>/<name>.img`, the name as the runtime allows it.
	volumeRe   = regexp.MustCompile(`^t-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/[A-Za-z0-9][A-Za-z0-9_.-]{0,62}\.img$`)
	transferRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidVolume says whether s names a volume image: `t-<tenant uuid>/<name>.img`, with no `..` anywhere.
func ValidVolume(s string) bool { return volumeRe.MatchString(s) && !strings.Contains(s, "..") }

// ValidTransferID says whether s is a transfer's id: a lower-case UUID.
func ValidTransferID(s string) bool { return transferRe.MatchString(s) }

// ValidSHA256 says whether s is a SHA-256 in lower-case hex.
func ValidSHA256(s string) bool { return sha256Re.MatchString(s) }

// ExportParams are what a volume-export task carries.
type ExportParams struct {
	TransferID string
	Volume     string
}

// ImportParams are what a volume-import task carries.
type ImportParams struct {
	TransferID string
	Volume     string
	Size       int64
	SHA256     string
	Chunks     int
}

// RemoveParams are what a volume-remove task carries; SHA256 is optional.
type RemoveParams struct {
	Volume string
	SHA256 string
}

// only refuses any key that is not one of allowed: a key the agent does not understand is never silently ignored.
func only(task string, m map[string]any, allowed ...string) error {
	for k := range m {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok {
			return fmt.Errorf("%s carries only %s, not %q", task, strings.Join(allowed, ", "), k)
		}
	}
	return nil
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func volumeParam(m map[string]any) (string, error) {
	v := str(m, "volume")
	if !ValidVolume(v) {
		return "", errors.New("the volume is not t-<tenant uuid>/<name>.img")
	}
	return v, nil
}

func transferParam(m map[string]any) (string, error) {
	id := str(m, "transferId")
	if !ValidTransferID(id) {
		return "", errors.New("the transferId is not a lower-case UUID")
	}
	return id, nil
}

// whole reads a JSON number that must be a whole number from lo to hi.
func whole(m map[string]any, k string, lo, hi float64) (float64, bool) {
	f, ok := m[k].(float64)
	return f, ok && f == math.Trunc(f) && f >= lo && f <= hi
}

// ParseExport checks a volume-export task's params: exactly transferId and volume.
func ParseExport(m map[string]any) (ExportParams, error) {
	if err := only("volume-export", m, "transferId", "volume"); err != nil {
		return ExportParams{}, err
	}
	id, err := transferParam(m)
	if err != nil {
		return ExportParams{}, err
	}
	v, err := volumeParam(m)
	if err != nil {
		return ExportParams{}, err
	}
	return ExportParams{TransferID: id, Volume: v}, nil
}

// ParseImport checks a volume-import task's params: exactly transferId, volume, size (1 byte to 1 TiB), sha256 and chunks (1 to MaxChunks).
func ParseImport(m map[string]any) (ImportParams, error) {
	if err := only("volume-import", m, "transferId", "volume", "size", "sha256", "chunks"); err != nil {
		return ImportParams{}, err
	}
	id, err := transferParam(m)
	if err != nil {
		return ImportParams{}, err
	}
	v, err := volumeParam(m)
	if err != nil {
		return ImportParams{}, err
	}
	size, ok := whole(m, "size", 1, float64(MaxSize))
	if !ok {
		return ImportParams{}, errors.New("the size is not a whole number of bytes from 1 to 1 TiB")
	}
	sum := str(m, "sha256")
	if !ValidSHA256(sum) {
		return ImportParams{}, errors.New("the sha256 is not 64 lower-case hex characters")
	}
	chunks, ok := whole(m, "chunks", 1, MaxChunks)
	if !ok {
		return ImportParams{}, fmt.Errorf("chunks is not a whole number from 1 to %d", MaxChunks)
	}
	return ImportParams{TransferID: id, Volume: v, Size: int64(size), SHA256: sum, Chunks: int(chunks)}, nil
}

// ParseRemove checks a volume-remove task's params: volume, and optionally sha256.
func ParseRemove(m map[string]any) (RemoveParams, error) {
	if err := only("volume-remove", m, "volume", "sha256"); err != nil {
		return RemoveParams{}, err
	}
	v, err := volumeParam(m)
	if err != nil {
		return RemoveParams{}, err
	}
	p := RemoveParams{Volume: v}
	if raw, ok := m["sha256"]; ok {
		s, _ := raw.(string)
		if !ValidSHA256(s) {
			return RemoveParams{}, errors.New("the sha256 is not 64 lower-case hex characters")
		}
		p.SHA256 = s
	}
	return p, nil
}

// Platform is the agent's client: the platform's routes, called with the agent's own credential.
type Platform interface {
	PostJSON(ctx context.Context, path string, body any, maxBytes int64, out any) error
}

// Mover does the three tasks against one volumes root.
type Mover struct {
	// Root is the volumes root; empty means DefaultRoot.
	Root     string
	Platform Platform
	// Wait is the pause before trying a chunk again (attempt counts from 1); nil waits 1, 2 and 4 seconds.
	Wait func(ctx context.Context, attempt int) error
}

// ExportResult is what volume-export reports.
type ExportResult struct {
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
	Chunks          int    `json:"chunks"`
	CompressedBytes int64  `json:"compressedBytes"`
}

// ImportResult is what volume-import reports.
type ImportResult struct {
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Already bool   `json:"already"`
}

// RemoveResult is what volume-remove reports.
type RemoveResult struct {
	Removed bool `json:"removed"`
}

func (m Mover) root() string {
	if m.Root == "" {
		return DefaultRoot
	}
	return m.Root
}

// path is where a (validated) volume is: its tenant directory and its image.
func (m Mover) path(volume string) (dir, file string) {
	file = filepath.Join(m.root(), filepath.FromSlash(volume))
	return filepath.Dir(file), file
}

var (
	errMissing = errors.New("missing")
	errInUse   = errors.New("in use")
)

// checkDir refuses a tenant directory that is not a plain directory (a symbolic link could lead outside the volumes root). A missing one is
// errMissing.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return errMissing
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", filepath.Base(dir))
	}
	return nil
}

// openLocked opens an image and takes the exclusive lock the runtime's VMM takes, without waiting: errMissing when there is no image,
// errInUse when a pod holds it. The lock is held until the file is closed. The file must be a regular file (not followed through a link),
// and still be the one at the path once the lock is held.
func openLocked(dir, file string) (*os.File, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errMissing
	}
	if err != nil {
		return nil, fmt.Errorf("could not open the image: %w", err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("the image is not a regular file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errInUse
		}
		return nil, fmt.Errorf("could not lock the image: %w", err)
	}
	now, err := os.Lstat(file)
	if err != nil || !os.SameFile(fi, now) {
		f.Close()
		return nil, errors.New("the image was replaced while it was being opened; try again")
	}
	return f, nil
}

// ctxReader stops a long read when the task's time is up.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// hashFile is the SHA-256 of the whole file and its length, read from the start.
func hashFile(ctx context.Context, f *os.File) (string, int64, error) {
	h := sha256.New()
	n, err := io.CopyBuffer(h, ctxReader{ctx, io.NewSectionReader(f, 0, math.MaxInt64)}, make([]byte, 1<<20))
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// syncDir makes a name added to or removed from dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// call makes one platform call, trying again after a network error or a 5xx (at most `retries` more times, after a short pause). A refusal
// (4xx), or an answer that cannot be read, ends it at once.
func (m Mover) call(ctx context.Context, what, path string, body any, maxBytes int64, out any) error {
	if m.Platform == nil {
		return errors.New("this agent was built without a way to reach the platform for a volume")
	}
	for attempt := 0; ; attempt++ {
		err := m.Platform.PostJSON(ctx, path, body, maxBytes, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: the task ran out of time", what)
		}
		var se *client.StatusError
		retryable := !errors.Is(err, client.ErrTooLarge) && !errors.Is(err, client.ErrUnreadable) &&
			(!errors.As(err, &se) || se.Status >= 500)
		if !retryable {
			if se != nil {
				return fmt.Errorf("%s: the platform refused it (%d): %s", what, se.Status, clip(se.Message))
			}
			return fmt.Errorf("%s: %w", what, err)
		}
		if attempt == retries {
			return fmt.Errorf("%s: failed %d times, the last: %w", what, retries+1, err)
		}
		if werr := m.wait(ctx, attempt+1); werr != nil {
			return fmt.Errorf("%s: the task ran out of time", what)
		}
	}
}

func (m Mover) wait(ctx context.Context, attempt int) error {
	if m.Wait != nil {
		return m.Wait(ctx, attempt)
	}
	t := time.NewTimer(time.Duration(1<<(attempt-1)) * time.Second)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// clip keeps the platform's error text short and on one line.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// putBody is one chunk sent to the platform.
type putBody struct {
	TransferID string `json:"transferId"`
	Index      int    `json:"index"`
	Data       string `json:"data"`
	SHA256     string `json:"sha256"`
}

// uploader cuts what is written to it into chunks of ChunkBytes and sends each, in order.
type uploader struct {
	ctx      context.Context
	m        Mover
	transfer string
	buf      []byte
	sent     int
	total    int64
}

func (u *uploader) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		take := min(ChunkBytes-len(u.buf), len(p))
		u.buf = append(u.buf, p[:take]...)
		p = p[take:]
		if len(u.buf) == ChunkBytes {
			if err := u.flush(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

func (u *uploader) flush() error {
	if len(u.buf) == 0 {
		return nil
	}
	sum := sha256.Sum256(u.buf)
	body := putBody{TransferID: u.transfer, Index: u.sent, Data: base64.StdEncoding.EncodeToString(u.buf), SHA256: hex.EncodeToString(sum[:])}
	if err := u.m.call(u.ctx, fmt.Sprintf("sending chunk %d", u.sent), PutPath, body, 0, nil); err != nil {
		return err
	}
	u.sent++
	u.total += int64(len(u.buf))
	u.buf = u.buf[:0]
	return nil
}

// Export reads one image no pod holds, holding its lock for the whole read, and sends it to the platform gzipped, in chunks.
func (m Mover) Export(ctx context.Context, p ExportParams) (ExportResult, error) {
	dir, file := m.path(p.Volume)
	f, err := openLocked(dir, file)
	switch {
	case errors.Is(err, errMissing):
		return ExportResult{}, fmt.Errorf("no volume %s on this machine", p.Volume)
	case errors.Is(err, errInUse):
		return ExportResult{}, fmt.Errorf("%s is in use by a running pod", p.Volume)
	case err != nil:
		return ExportResult{}, fmt.Errorf("%s: %w", p.Volume, err)
	}
	defer f.Close() // and with it the lock
	up := &uploader{ctx: ctx, m: m, transfer: p.TransferID, buf: make([]byte, 0, ChunkBytes)}
	gz := gzip.NewWriter(up)
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(gz, h), ctxReader{ctx, f}, make([]byte, 1<<20))
	if err != nil {
		return ExportResult{}, fmt.Errorf("reading %s: %w", p.Volume, err)
	}
	if err := gz.Close(); err != nil {
		return ExportResult{}, fmt.Errorf("compressing %s: %w", p.Volume, err)
	}
	if err := up.flush(); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), Chunks: up.sent, CompressedBytes: up.total}, nil
}

// getAnswer is one chunk fetched from the platform.
type getAnswer struct {
	Data   string `json:"data"`
	SHA256 string `json:"sha256"`
}

// downloader reads a transfer's chunks from the platform, in order, each checked against its hash, and ends after the last one.
type downloader struct {
	ctx      context.Context
	m        Mover
	transfer string
	chunks   int
	next     int
	buf      []byte
}

func (d *downloader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.next == d.chunks {
			return 0, io.EOF
		}
		if err := d.fetch(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

func (d *downloader) fetch() error {
	var a getAnswer
	what := fmt.Sprintf("fetching chunk %d", d.next)
	if err := d.m.call(d.ctx, what, GetPath, map[string]any{"transferId": d.transfer, "index": d.next}, maxGetAnswer, &a); err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil || len(data) == 0 || len(data) > ChunkBytes {
		return fmt.Errorf("chunk %d from the platform is not 1 to %d bytes of base64", d.next, ChunkBytes)
	}
	sum := sha256.Sum256(data)
	if !ValidSHA256(a.SHA256) || hex.EncodeToString(sum[:]) != a.SHA256 {
		return fmt.Errorf("chunk %d from the platform does not match its checksum", d.next)
	}
	d.buf = data
	d.next++
	return nil
}

// writeSparse writes what r gives into f from the start, leaving every all-zero block as a hole, and returns how many bytes it gave. It
// stops with an error as soon as r gives more than limit bytes.
func writeSparse(r io.Reader, f *os.File, limit int64) (int64, error) {
	buf := make([]byte, 1<<20)
	zero := make([]byte, block)
	var off int64
	for {
		n, err := fill(r, buf)
		if n > 0 {
			if off+int64(n) > limit {
				return off + int64(n), fmt.Errorf("the platform's copy is larger than the %d bytes signed", limit)
			}
			// write each run of non-zero blocks at once; skip each zero block
			start := -1
			for i := 0; i < n; i += block {
				end := min(i+block, n)
				if bytes.Equal(buf[i:end], zero[:end-i]) {
					if start >= 0 {
						if _, werr := f.WriteAt(buf[start:i], off+int64(start)); werr != nil {
							return off, werr
						}
						start = -1
					}
					continue
				}
				if start < 0 {
					start = i
				}
			}
			if start >= 0 {
				if _, werr := f.WriteAt(buf[start:n], off+int64(start)); werr != nil {
					return off, werr
				}
			}
			off += int64(n)
		}
		if err == io.EOF {
			return off, nil
		}
		if err != nil {
			return off, err
		}
	}
}

// fill reads until buf is full or r ends. Only a clean end is io.EOF; anything else r says (a gzip stream cut short is
// io.ErrUnexpectedEOF) is returned as it is, unlike io.ReadFull, which would make a cut stream look like a clean end.
func fill(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Import writes the image the platform holds for this machine under a temporary name, checks its size and SHA-256 against the signed ones,
// and only then puts it in place. An image already there is never overwritten: if it is this very copy (an import tried again) the import
// is done; otherwise it fails and touches nothing.
func (m Mover) Import(ctx context.Context, p ImportParams) (ImportResult, error) {
	dir, file := m.path(p.Volume)
	temp := filepath.Join(dir, "."+filepath.Base(file)+".incoming-"+p.TransferID)
	if res, done, err := m.existing(ctx, dir, file, p); done || err != nil {
		if err == nil {
			_ = os.Remove(temp) // left by an attempt that put the image in place and stopped before tidying up
		}
		return res, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ImportResult{}, fmt.Errorf("could not make the volume's directory: %w", err)
	}
	if err := checkDir(dir); err != nil {
		return ImportResult{}, fmt.Errorf("the volume's directory: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	// a temporary file of this transfer can only be left from an attempt that did not finish
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ImportResult{}, fmt.Errorf("could not clear an earlier attempt: %w", err)
	}
	f, err := os.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ImportResult{}, fmt.Errorf("could not create the temporary image: %w", err)
	}
	placed := false
	defer func() {
		f.Close()
		if !placed {
			_ = os.Remove(temp)
		}
	}()

	down := &downloader{ctx: ctx, m: m, transfer: p.TransferID, chunks: p.Chunks}
	gz, err := gzip.NewReader(down)
	if err != nil {
		return ImportResult{}, fmt.Errorf("the platform's copy of %s: %w", p.Volume, wrapGzip(err))
	}
	h := sha256.New()
	n, err := writeSparse(ctxReader{ctx, io.TeeReader(gz, h)}, f, p.Size)
	if err != nil {
		return ImportResult{}, fmt.Errorf("writing %s: %w", p.Volume, wrapGzip(err))
	}
	if down.next != p.Chunks || len(down.buf) != 0 {
		return ImportResult{}, fmt.Errorf("the platform's copy of %s ended before its last chunk", p.Volume)
	}
	if err := f.Truncate(p.Size); err != nil {
		return ImportResult{}, fmt.Errorf("sizing %s: %w", p.Volume, err)
	}
	if err := f.Sync(); err != nil {
		return ImportResult{}, fmt.Errorf("saving %s: %w", p.Volume, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if n != p.Size || sum != p.SHA256 {
		return ImportResult{}, fmt.Errorf("the copy of %s is %d bytes with SHA-256 %s, not the %d bytes and %s signed; nothing was put in place", p.Volume, n, sum[:12], p.Size, p.SHA256[:12])
	}
	// put in place without ever replacing: a link fails if the name is taken (the runtime creates images the same way)
	if err := os.Link(temp, file); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ImportResult{}, fmt.Errorf("a volume %s appeared on this machine while it was being written; nothing was overwritten", p.Volume)
		}
		return ImportResult{}, fmt.Errorf("putting %s in place: %w", p.Volume, err)
	}
	placed = true
	_ = os.Remove(temp)
	if err := syncDir(dir); err != nil {
		return ImportResult{}, fmt.Errorf("saving %s in its directory: %w", p.Volume, err)
	}
	return ImportResult{Size: n, SHA256: sum, Already: false}, nil
}

// wrapGzip says that a stream that is not gzip, or is cut short, is the platform's copy that is wrong.
func wrapGzip(err error) error {
	if errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("the platform's copy is not a whole gzip stream (%v)", err)
	}
	return err
}

// existing looks at an image already where an import would put it: done when it is this very copy, an error when it is any other.
func (m Mover) existing(ctx context.Context, dir, file string, p ImportParams) (ImportResult, bool, error) {
	f, err := openLocked(dir, file)
	switch {
	case errors.Is(err, errMissing):
		return ImportResult{}, false, nil
	case errors.Is(err, errInUse):
		return ImportResult{}, false, fmt.Errorf("a volume %s is already on this machine and in use by a running pod; nothing was changed", p.Volume)
	case err != nil:
		return ImportResult{}, false, fmt.Errorf("%s: %w", p.Volume, err)
	}
	defer f.Close()
	different := fmt.Errorf("a different volume %s is already on this machine; nothing was changed", p.Volume)
	fi, err := f.Stat()
	if err != nil {
		return ImportResult{}, false, err
	}
	if fi.Size() != p.Size {
		return ImportResult{}, false, different
	}
	sum, n, err := hashFile(ctx, f)
	if err != nil {
		return ImportResult{}, false, fmt.Errorf("reading the volume %s already here: %w", p.Volume, err)
	}
	if sum != p.SHA256 || n != p.Size {
		return ImportResult{}, false, different
	}
	return ImportResult{Size: n, SHA256: sum, Already: true}, true, nil
}

// Remove deletes one image no pod holds, under its lock; with a checksum, only if the image is that copy. No image is not a failure.
func (m Mover) Remove(ctx context.Context, p RemoveParams) (RemoveResult, error) {
	dir, file := m.path(p.Volume)
	f, err := openLocked(dir, file)
	switch {
	case errors.Is(err, errMissing):
		return RemoveResult{Removed: false}, nil
	case errors.Is(err, errInUse):
		return RemoveResult{}, fmt.Errorf("%s is in use by a running pod; nothing was removed", p.Volume)
	case err != nil:
		return RemoveResult{}, fmt.Errorf("%s: %w", p.Volume, err)
	}
	defer f.Close()
	if p.SHA256 != "" {
		sum, _, err := hashFile(ctx, f)
		if err != nil {
			return RemoveResult{}, fmt.Errorf("reading %s: %w", p.Volume, err)
		}
		if sum != p.SHA256 {
			return RemoveResult{}, fmt.Errorf("%s is not the copy the platform expects (its SHA-256 is %s, not %s); nothing was removed", p.Volume, sum[:12], p.SHA256[:12])
		}
	}
	if err := os.Remove(file); err != nil {
		return RemoveResult{}, fmt.Errorf("removing %s: %w", p.Volume, err)
	}
	if err := syncDir(dir); err != nil {
		return RemoveResult{}, fmt.Errorf("saving the removal of %s: %w", p.Volume, err)
	}
	return RemoveResult{Removed: true}, nil
}
