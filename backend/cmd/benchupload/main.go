// Command benchupload measures a single-stream tus upload the way the web
// UI makes it (frontend/src/uploads.tsx: one PATCH per 64 MiB chunk) and
// its resumption after a broken connection — PRD G1, docs/mvp-tasks.md
// E3-9. bench/README.md runs it behind an emulated 1 Gbit/s link.
//
// The content is generated on the fly from a seeded PRNG: a 10 GB file
// never touches the client's disk, and the server checks the sha256 the
// client announces, so a byte lost or doubled on resume fails the run.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// gigabit is 1 Gbit/s in bytes per second, what G1 is measured against.
const gigabit = 125e6

type options struct {
	baseURL    string
	session    string
	cookieName string
	user       string
	password   string
	size       int64
	chunk      int64
	seed       uint64
	breakAt    float64
	cleanup    bool
	label      string
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	var o options
	size, chunk := byteSize(10<<30), byteSize(64<<20)
	flag.StringVar(&o.baseURL, "url", "http://localhost:8090", "server base URL")
	// Secrets default from the environment so they stay out of ps output.
	flag.StringVar(&o.session, "session", os.Getenv("SESSION"), "session cookie value (default $SESSION)")
	flag.StringVar(&o.cookieName, "cookie-name", "session", "session cookie name (__Host-session behind https)")
	flag.StringVar(&o.user, "user", os.Getenv("BENCH_USER"), "local user to sign in as instead of -session (default $BENCH_USER)")
	flag.StringVar(&o.password, "password", os.Getenv("BENCH_PASSWORD"), "password for -user (default $BENCH_PASSWORD)")
	flag.Var(&size, "size", "file size: bytes or with KiB/MiB/GiB/KB/MB/GB")
	flag.Var(&chunk, "chunk", "bytes per PATCH, as the web UI's chunkSize")
	flag.Uint64Var(&o.seed, "seed", 1, "content seed: the same seed and size give the same bytes")
	flag.Float64Var(&o.breakAt, "break-at", 0, "drop the connection once this fraction is sent (0..1, 0 = never) and resume")
	flag.BoolVar(&o.cleanup, "cleanup", true, "delete the file for good afterwards (trash, then purge), or the upload on failure")
	flag.StringVar(&o.label, "label", "", "free-form label copied to the JSON result (e.g. the RTT)")
	flag.Parse()
	o.size, o.chunk = int64(size), int64(chunk)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o); err != nil {
		slog.Error("benchupload failed", "err", err)
		os.Exit(1)
	}
}

// result is the JSON line a run ends with. MB are 10^6 bytes, as link
// speeds are counted.
type result struct {
	Label       string  `json:"label"`
	Size        int64   `json:"size"`
	Chunk       int64   `json:"chunk"`
	HashSeconds float64 `json:"hashSeconds"`
	// TransferSeconds runs from the first PATCH to the last byte handed
	// to the connection, FinalizeSeconds from there to the response that
	// completes the upload: the server's hashing and commit.
	TransferSeconds float64 `json:"transferSeconds"`
	FinalizeSeconds float64 `json:"finalizeSeconds"`
	// TotalSeconds is from the creating POST to the completing response.
	TotalSeconds    float64  `json:"totalSeconds"`
	MBps            float64  `json:"mbps"`
	PctOf1Gbit      float64  `json:"pctOf1Gbit"`
	MBpsTotal       float64  `json:"mbpsTotal"`
	PctOf1GbitTotal float64  `json:"pctOf1GbitTotal"`
	Resumed         bool     `json:"resumed"`
	Resumes         []resume `json:"resumes,omitempty"`
	ResentBytes     int64    `json:"resentBytes"`
	NodeID          string   `json:"nodeId"`
	SHA256OK        bool     `json:"sha256Ok"`
}

// resume is one recovery: how far the client had sent when the request
// failed and the offset the server said it kept.
type resume struct {
	Sent  int64  `json:"sent"`
	Kept  int64  `json:"kept"`
	Cause string `json:"cause"`
}

func run(ctx context.Context, o options) error {
	if o.size <= 0 || o.chunk <= 0 {
		return errors.New("-size and -chunk must be positive")
	}
	if o.breakAt < 0 || o.breakAt >= 1 {
		return errors.New("-break-at must be in [0, 1)")
	}
	base, err := url.Parse(strings.TrimSuffix(o.baseURL, "/"))
	if err != nil {
		return fmt.Errorf("-url: %w", err)
	}
	c := newClient(base, o.cookieName, o.session)
	if o.session == "" {
		if o.user == "" || o.password == "" {
			return errors.New("set -session ($SESSION) or -user and -password")
		}
		if err := c.login(ctx, o.user, o.password); err != nil {
			return err
		}
	}

	// The checksum is a separate pass, not part of the measurement: a
	// client hashing while it sends would only add its own CPU to the
	// server's.
	hstart := time.Now()
	h := sha256.New()
	if _, err := newStream(o.seed, o.size, 0).WriteTo(h); err != nil {
		return err
	}
	sum := h.Sum(nil)
	hashTime := time.Since(hstart)
	slog.Info("content hashed", "size", o.size, "sha256", hex.EncodeToString(sum), "took", hashTime.Round(time.Millisecond),
		"MBps", round1(float64(o.size)/hashTime.Seconds()/1e6))

	name := fmt.Sprintf("benchupload-%d-%d.bin", o.seed, time.Now().Unix())
	up := &uploader{c: c, size: o.size, chunk: o.chunk, seed: o.seed}
	if o.breakAt > 0 {
		up.breakAt = breakPoint(o.breakAt, o.size, o.chunk)
	}
	res, err := up.run(ctx, name, sum)
	if err != nil {
		if up.location != "" && o.cleanup {
			// Releases the quota reservation and the staged bytes now
			// rather than when the reservation lapses.
			if terr := c.terminate(context.WithoutCancel(ctx), up.location); terr != nil {
				slog.Warn("could not delete the unfinished upload", "upload", up.location, "err", terr)
			}
		}
		return err
	}
	res.Label, res.HashSeconds = o.label, round3(hashTime.Seconds())

	fmt.Fprintf(os.Stderr, "\n%s: %s in %.1f s (transfer %.1f s + finalization %.1f s)\n",
		labelOr(o.label), humanSize(o.size), res.TotalSeconds, res.TransferSeconds, res.FinalizeSeconds)
	fmt.Fprintf(os.Stderr, "  transfer %.1f MB/s = %.0f%% of 1 Gbit/s; end to end %.1f MB/s = %.0f%%\n",
		res.MBps, res.PctOf1Gbit, res.MBpsTotal, res.PctOf1GbitTotal)
	for _, r := range res.Resumes {
		fmt.Fprintf(os.Stderr, "  resumed after %s: sent %d, server kept %d (lost in flight %d)\n", r.Cause, r.Sent, r.Kept, r.Sent-r.Kept)
	}
	fmt.Fprintf(os.Stderr, "  re-sent %d bytes; node %s, sha256 checked by the server\n\n", res.ResentBytes, res.NodeID)

	if o.cleanup && res.NodeID != "" {
		if err := c.purge(context.WithoutCancel(ctx), res.NodeID); err != nil {
			return fmt.Errorf("cleanup (node %s is left behind): %w", res.NodeID, err)
		}
		slog.Info("cleaned up", "node", res.NodeID)
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

// breakPoint is the offset to drop the connection at, frac of the way
// through. One on a chunk boundary would cut no request — the PATCH before
// it is complete, the next not yet begun — so it moves into that chunk.
func breakPoint(frac float64, size, chunk int64) int64 {
	at := int64(frac * float64(size))
	if at%chunk == 0 {
		at += min(chunk, size-at) / 2
	}
	return max(at, 1)
}

// retryDelays are tus-js-client's as the web UI sets them: a server
// restart is ridden out, a server that stays down is not.
var retryDelays = []time.Duration{0, time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second}

type uploader struct {
	c           *client
	size, chunk int64
	seed        uint64
	// breakAt is the offset to drop the connection at, 0 once done.
	breakAt  int64
	location string

	// sent counts every byte handed to a PATCH body, re-sent ones too;
	// inFlight is what the last PATCH took before it ended.
	sent     int64
	inFlight int64
	lastByte time.Time
}

// errFatal marks a failure no retry would fix.
type errFatal struct{ err error }

func (e errFatal) Error() string { return e.err.Error() }
func (e errFatal) Unwrap() error { return e.err }

func (u *uploader) run(ctx context.Context, name string, sum []byte) (result, error) {
	start := time.Now()
	loc, err := u.c.create(ctx, u.size, map[string]string{
		"filename":   name,
		"parentId":   "root",
		"sha256":     hex.EncodeToString(sum),
		"onConflict": "rename",
	})
	if err != nil {
		return result{}, err
	}
	u.location = loc
	slog.Info("upload created", "location", loc)

	var (
		offset    int64
		nodeID    string
		resumes   []resume
		attempt   int
		needHead  bool
		lastCause string
		nextLog   = u.size / 10
	)
	first := time.Now()
	for {
		if needHead {
			kept, err := u.c.head(ctx, loc)
			if err != nil {
				if ctx.Err() != nil || errors.As(err, new(errFatal)) {
					return result{}, err
				}
				if attempt, err = backoff(ctx, attempt, err); err != nil {
					return result{}, err
				}
				continue
			}
			resumes = append(resumes, resume{Sent: offset + u.inFlight, Kept: kept, Cause: lastCause})
			slog.Info("resuming", "sent", offset+u.inFlight, "server_offset", kept)
			offset, needHead, u.inFlight = kept, false, 0
		}
		n := min(u.chunk, u.size-offset)
		newOffset, id, err := u.patch(ctx, loc, offset, n)
		if err == nil {
			offset, attempt = newOffset, 0
			if offset >= nextLog && offset < u.size {
				slog.Info("progress", "pct", 100*offset/u.size, "MBps", round1(float64(offset)/time.Since(first).Seconds()/1e6))
				nextLog = offset + u.size/10
			}
			if offset == u.size {
				nodeID = id
				break
			}
			continue
		}
		if ctx.Err() != nil || errors.As(err, new(errFatal)) {
			return result{}, err
		}
		lastCause = err.Error()
		slog.Warn("PATCH failed, will ask the server for its offset", "offset", offset, "err", err)
		needHead = true
		if attempt, err = backoff(ctx, attempt, err); err != nil {
			return result{}, err
		}
	}
	done := time.Now()

	res := result{
		Size:            u.size,
		Chunk:           u.chunk,
		TransferSeconds: round3(u.lastByte.Sub(first).Seconds()),
		FinalizeSeconds: round3(done.Sub(u.lastByte).Seconds()),
		TotalSeconds:    round3(done.Sub(start).Seconds()),
		Resumed:         len(resumes) > 0,
		Resumes:         resumes,
		ResentBytes:     u.sent - u.size,
		NodeID:          nodeID,
		// The completing PATCH only succeeds once the server's hash of
		// the stored file matched the announced one.
		SHA256OK: true,
	}
	transfer, total := u.lastByte.Sub(first).Seconds(), done.Sub(start).Seconds()
	res.MBps = round1(float64(u.size) / transfer / 1e6)
	res.PctOf1Gbit = round1(100 * float64(u.size) / transfer / gigabit)
	res.MBpsTotal = round1(float64(u.size) / total / 1e6)
	res.PctOf1GbitTotal = round1(100 * float64(u.size) / total / gigabit)
	return res, nil
}

// backoff waits out the next retry delay, or gives up after the last.
func backoff(ctx context.Context, attempt int, cause error) (int, error) {
	if attempt >= len(retryDelays) {
		return attempt, fmt.Errorf("giving up after %d retries: %w", attempt, cause)
	}
	select {
	case <-time.After(retryDelays[attempt]):
	case <-ctx.Done():
		return attempt, ctx.Err()
	}
	return attempt + 1, nil
}

// patch sends n bytes from offset as one PATCH and returns the server's
// new offset and, once complete, the node the upload became. With a break
// point inside the chunk the request is cancelled there, mid-body, the way
// a dropped connection cuts it.
func (u *uploader) patch(ctx context.Context, loc string, offset, n int64) (int64, string, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := &countingReader{r: io.LimitReader(newStream(u.seed, u.size, offset), n)}
	if u.breakAt > offset && u.breakAt < offset+n {
		body.cutAfter, body.cut = u.breakAt-offset, cancel
		u.breakAt = 0
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPatch, loc, body)
	if err != nil {
		return 0, "", err
	}
	req.ContentLength = n
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	resp, err := u.c.do(req)
	u.sent += body.n
	u.inFlight = body.n
	if body.cutAt > 0 {
		if err == nil {
			drain(resp)
		}
		return 0, "", fmt.Errorf("connection dropped on purpose at %d", offset+body.cutAt)
	}
	if err != nil {
		return 0, "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent {
		return 0, "", responseError("PATCH", resp)
	}
	got, err := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		return 0, "", errFatal{fmt.Errorf("PATCH: bad Upload-Offset %q", resp.Header.Get("Upload-Offset"))}
	}
	if got == u.size {
		u.lastByte = body.done
		if u.lastByte.IsZero() {
			u.lastByte = time.Now()
		}
	}
	return got, resp.Header.Get("Upload-Node-Id"), nil
}

// countingReader is a PATCH body that counts what the transport takes
// from it, remembers when it ran dry, and can cut the request short.
type countingReader struct {
	r    io.Reader
	n    int64
	done time.Time

	cutAfter int64
	cut      context.CancelFunc
	cutAt    int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.cut != nil && c.n+int64(len(p)) > c.cutAfter {
		p = p[:c.cutAfter-c.n]
		if len(p) == 0 {
			c.cutAt = c.n
			c.cut()
			c.cut = nil
			return 0, context.Canceled
		}
	}
	k, err := c.r.Read(p)
	c.n += int64(k)
	if err == io.EOF && c.done.IsZero() {
		c.done = time.Now()
	}
	return k, err
}

// stream is the upload's content. Block i is ChaCha8 output keyed by
// (seed, i), so the bytes at any offset can be made again without those
// before it — a resume starts wherever the server stopped — and random
// bytes don't compress, so nothing on the way can shrink them.
type stream struct {
	rng       rand.ChaCha8
	seed      uint64
	size, off int64
	block     []byte
	blockNo   int64
}

const blockSize = 1 << 20

func newStream(seed uint64, size, off int64) *stream {
	return &stream{seed: seed, size: size, off: off, block: make([]byte, blockSize), blockNo: -1}
}

// current fills the block holding s.off and returns its rest.
func (s *stream) current() []byte {
	no := s.off / blockSize
	if no != s.blockNo {
		var key [32]byte
		binary.LittleEndian.PutUint64(key[:8], s.seed)
		binary.LittleEndian.PutUint64(key[8:16], uint64(no))
		s.rng.Seed(key)
		_, _ = s.rng.Read(s.block)
		s.blockNo = no
	}
	rest := s.block[s.off%blockSize:]
	if left := s.size - s.off; int64(len(rest)) > left {
		rest = rest[:left]
	}
	return rest
}

func (s *stream) Read(p []byte) (int, error) {
	if s.off >= s.size {
		return 0, io.EOF
	}
	n := copy(p, s.current())
	s.off += int64(n)
	return n, nil
}

// WriteTo hands whole blocks over, without Read's copy: hashing 10 GB
// shouldn't take longer than it must.
func (s *stream) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for s.off < s.size {
		n, err := w.Write(s.current())
		s.off += int64(n)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// client talks to the server as a signed-in user.
type client struct {
	base   *url.URL
	http   *http.Client
	cookie *http.Cookie
}

func newClient(base *url.URL, cookieName, session string) *client {
	return &client{
		base: base,
		http: &http.Client{Transport: &http.Transport{
			// Never through a proxy: it would be measured along with the
			// server.
			Proxy:              nil,
			DisableCompression: true,
			// Fewer, larger writes than the default 4 KiB.
			WriteBufferSize: 256 << 10,
			// The completing PATCH waits for the server to hash the file.
			ResponseHeaderTimeout: 10 * time.Minute,
		}},
		cookie: &http.Cookie{Name: cookieName, Value: session},
	}
}

func (c *client) url(path string) string { return c.base.String() + path }

func (c *client) do(req *http.Request) (*http.Response, error) {
	req.AddCookie(c.cookie)
	if strings.HasPrefix(req.URL.Path, "/api/v1/uploads") {
		req.Header.Set("Tus-Resumable", "1.0.0")
	}
	return c.http.Do(req)
}

// login signs in with a local account (POST /api/v1/auth/login).
func (c *client) login(ctx context.Context, user, password string) error {
	body, _ := json.Marshal(map[string]string{"username": user, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v1/auth/login"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return responseError("login", resp)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == c.cookie.Name {
			c.cookie.Value = ck.Value
			return nil
		}
	}
	return fmt.Errorf("login: no %s cookie in the response (set -cookie-name)", c.cookie.Name)
}

// create starts a tus upload and returns its URL.
func (c *client) create(ctx context.Context, size int64, meta map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v1/uploads/"), nil)
	if err != nil {
		return "", err
	}
	pairs := make([]string, 0, len(meta))
	for k, v := range meta {
		pairs = append(pairs, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	req.Header.Set("Upload-Length", strconv.FormatInt(size, 10))
	req.Header.Set("Upload-Metadata", strings.Join(pairs, ","))
	resp, err := c.do(req)
	if err != nil {
		return "", fmt.Errorf("create upload: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return "", responseError("create upload", resp)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("create upload: %w", err)
	}
	return loc.String(), nil
}

// head asks the server how much of the upload it has.
func (c *client) head(ctx context.Context, loc string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, loc, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return 0, responseError("HEAD", resp)
	}
	off, err := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		return 0, errFatal{fmt.Errorf("HEAD: bad Upload-Offset %q", resp.Header.Get("Upload-Offset"))}
	}
	return off, nil
}

// terminate deletes an unfinished upload (tus termination).
func (c *client) terminate(ctx context.Context, loc string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, loc, nil)
	if err != nil {
		return err
	}
	return c.expect(req, http.StatusNoContent, "delete upload")
}

// purge moves the node to the trash and deletes it from there, as a user
// would in two steps: no API deletes past the trash in one.
func (c *client) purge(ctx context.Context, nodeID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url("/api/v1/nodes/"+url.PathEscape(nodeID)), nil)
	if err != nil {
		return err
	}
	if err := c.expect(req, http.StatusNoContent, "trash"); err != nil {
		return err
	}
	// Newest first: the item is on the first page.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, c.url("/api/v1/trash"), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("list trash: %w", err)
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	err = json.NewDecoder(resp.Body).Decode(&page)
	drain(resp)
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list trash: status %d, %v", resp.StatusCode, err)
	}
	for _, it := range page.Items {
		if it.ID != nodeID {
			continue
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodDelete, c.url("/api/v1/trash/"+url.PathEscape(it.ID)), nil)
		if err != nil {
			return err
		}
		return c.expect(req, http.StatusNoContent, "purge")
	}
	return fmt.Errorf("node %s is not in the trash", nodeID)
}

func (c *client) expect(req *http.Request, status int, what string) error {
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer drain(resp)
	if resp.StatusCode != status {
		return responseError(what, resp)
	}
	return nil
}

// responseError describes an unexpected response and decides whether a
// retry may help: like the web UI, it retries a server error, a wrong
// offset (409 from tusd) and a lock still held by the dropped request,
// not an error of the API's own — a checksum mismatch, a taken name.
func responseError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	err := fmt.Errorf("%s: %s: %s", what, resp.Status, strings.TrimSpace(string(body)))
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusLocked:
		return err
	case resp.StatusCode == http.StatusConflict && mt != "application/json":
		return err
	}
	return errFatal{err}
}

// drain lets the connection be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// byteSize is a flag taking "10GiB", "64MiB", "500MB" or plain bytes.
type byteSize int64

func (b *byteSize) String() string { return strconv.FormatInt(int64(*b), 10) }

func (b *byteSize) Set(s string) error {
	units := []struct {
		suffix string
		mult   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1}}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("not a size: %q", s)
	}
	*b = byteSize(n * mult)
	return nil
}

func humanSize(n int64) string {
	if n >= 1<<30 && n%(1<<30) == 0 {
		return fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("%.2f GB", float64(n)/1e9)
}

func labelOr(l string) string {
	if l == "" {
		return "upload"
	}
	return l
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }
