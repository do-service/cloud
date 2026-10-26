package engine

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/state"
)

// kill -9 of the engine in the middle of a 200 MB upload and a download:
// the next run resumes (tus from the server's offset, the download from
// the .part with Range) and the content arrives whole.

const helperEnv = "ENGINE_KILL_HELPER"

// helperMain is the child process: one SyncOnce of the folder in env,
// then how many bytes it sent and received on stdout.
func helperMain() int {
	limit, _ := strconv.Atoi(os.Getenv("HELPER_LIMIT"))
	n := &byteCounter{next: &http.Transport{Proxy: nil}}
	a, err := api.New(os.Getenv("HELPER_SERVER"), api.Options{
		Token: api.StaticToken(os.Getenv("HELPER_TOKEN")), HTTPClient: &http.Client{Transport: n},
		ChunkSize: 4 << 20, UploadLimit: api.NewLimiter(limit), DownloadLimit: api.NewLimiter(limit),
	})
	if err != nil {
		fmt.Println("error", err)
		return 1
	}
	ctx := context.Background()
	db, err := state.Open(ctx, os.Getenv("HELPER_DB"))
	if err != nil {
		fmt.Println("error", err)
		return 1
	}
	defer db.Close()
	e, err := New(Config{DB: db, Remote: a, Root: os.Getenv("HELPER_ROOT"), ConnID: testConn, UserID: os.Getenv("HELPER_USER"),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		fmt.Println("error", err)
		return 1
	}
	fmt.Println("started")
	if _, err := e.SyncOnce(ctx); err != nil {
		fmt.Println("error", err)
		return 1
	}
	fmt.Printf("done sent=%d got=%d\n", n.sent.Load(), n.got.Load())
	return 0
}

type byteCounter struct {
	next      http.RoundTripper
	sent, got atomic.Int64
}

func (c *byteCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPatch && r.ContentLength > 0 {
		c.sent.Add(r.ContentLength)
	}
	res, err := c.next.RoundTrip(r)
	if err == nil && strings.HasSuffix(r.URL.Path, "/content") {
		res.Body = &countingBody{ReadCloser: res.Body, n: &c.got}
	}
	return res, err
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

type child struct {
	cmd *exec.Cmd
	out *bufio.Scanner
}

func startChild(t *testing.T, env []string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), append(env, helperEnv+"=1")...)
	cmd.Stderr = os.Stderr
	if testing.Verbose() == false {
		cmd.Stderr = nil
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd, out: bufio.NewScanner(out)}
	if !c.out.Scan() || c.out.Text() != "started" {
		t.Fatalf("child: %q", c.out.Text())
	}
	return c
}

// finish waits for the child's report: bytes sent and received.
func (c *child) finish(t *testing.T) (sent, got int64) {
	t.Helper()
	for c.out.Scan() {
		line := c.out.Text()
		if strings.HasPrefix(line, "done ") {
			fmt.Sscanf(line, "done sent=%d got=%d", &sent, &got)
		}
		if strings.HasPrefix(line, "error") {
			t.Fatalf("child: %s", line)
		}
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	return sent, got
}

func (c *child) kill(t *testing.T) {
	if err := c.cmd.Process.Signal(os.Kill); err != nil {
		t.Fatal(err)
	}
	_ = c.cmd.Wait()
}

func bigFile(t *testing.T, path string, size int64) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	block := make([]byte, 1<<20)
	for written := int64(0); written < size; written += int64(len(block)) {
		_, _ = rand.Read(block)
		if _, err := io.MultiWriter(f, h).Write(block); err != nil {
			t.Fatal(err)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIntegrationKill9(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MB transfers")
	}
	const size = 200 << 20
	c := newCloud(t, 0)
	// The parent sets the folder up (marker, account, selection) and
	// lets go of the database; the engine then only runs in children.
	h := c.client("Мак", nil)
	h.sync()
	h.close()
	env := []string{"HELPER_ROOT=" + h.root, "HELPER_DB=" + h.dbPath, "HELPER_SERVER=" + c.srv.URL,
		"HELPER_USER=" + c.user.ID, "HELPER_TOKEN=" + c.srv.DeviceToken(t, c.user)}
	const slow = "HELPER_LIMIT=" + "41943040" // 40 MB/s: 5 s for the file

	progress := func() int64 {
		db, err := state.Open(context.Background(), h.dbPath)
		if err != nil {
			return 0
		}
		defer db.Close()
		var done int64
		_ = db.View(context.Background(), func(tx *state.Tx) error {
			ops, _ := tx.Ops()
			for _, o := range ops {
				if x, err := tx.Transfer(o.ID); err == nil {
					done = max(done, x.Done)
				}
			}
			return nil
		})
		return done
	}

	t.Run("upload", func(t *testing.T) {
		sum := bigFile(t, filepath.Join(h.root, "большой.bin"), size)
		ch := startChild(t, append(env, slow))
		waitFor(t, "50 MB uploaded", func() bool { return progress() >= 50<<20 })
		ch.kill(t)
		at := progress()
		ch = startChild(t, env)
		sent, _ := ch.finish(t)
		t.Logf("killed at %d MB; the second run sent %d MB", at>>20, sent>>20)
		if sent > size-at+(8<<20) {
			t.Errorf("sent %d bytes after a kill at %d: not resumed", sent, at)
		}
		third, _ := c.apiClient(api.Options{})
		if got := c.serverTree(third)["большой.bin"]; got != sum {
			t.Fatalf("server has %q, want %s", got, sum)
		}
	})

	t.Run("download", func(t *testing.T) {
		third, _ := c.apiClient(api.Options{})
		src := filepath.Join(t.TempDir(), "src.bin")
		sum := bigFile(t, src, size)
		f, err := os.Open(src)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := third.Upload(context.Background(), api.Upload{ParentID: c.nodeAt(third, "большой.bin").ParentID,
			Name: "с сервера.bin", Size: size, Body: f, OnConflict: api.ConflictFail}); err != nil {
			t.Fatal(err)
		}
		ch := startChild(t, append(env, slow))
		var part string
		waitFor(t, "50 MB downloaded", func() bool {
			ms, _ := filepath.Glob(filepath.Join(h.root, ".~cloud-*.part"))
			for _, m := range ms {
				if fi, err := os.Stat(m); err == nil && fi.Size() >= 50<<20 {
					part = m
					return true
				}
			}
			return false
		})
		ch.kill(t)
		fi, _ := os.Stat(part)
		ch = startChild(t, env)
		_, got := ch.finish(t)
		t.Logf("killed at %d MB; the second run downloaded %d MB", fi.Size()>>20, got>>20)
		if got > size-fi.Size()+(8<<20) {
			t.Errorf("downloaded %d bytes after a kill at %d: not resumed", got, fi.Size())
		}
		b, err := os.Open(filepath.Join(h.root, "с сервера.bin"))
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		hs := sha256.New()
		if _, err := io.Copy(hs, b); err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(hs.Sum(nil)) != sum {
			t.Fatal("downloaded content differs")
		}
		if ms, _ := filepath.Glob(filepath.Join(h.root, ".~cloud-*")); len(ms) != 0 {
			t.Fatalf("left behind: %v", ms)
		}
	})
}

// kill -9 in the middle of many small downloads, whose flushes are
// batched (localfs.Syncer): every file base records as synced is on disk
// with that content, nothing half-written is in place, and the next run
// finishes the rest. (A power cut is the case the flush order is for; it
// can't be cut here, the order is argued in localfs/durable.go.)
func TestIntegrationKill9SmallFiles(t *testing.T) {
	c := newCloud(t, 0)
	third, _ := c.apiClient(api.Options{})
	dir, err := third.Mkdir(context.Background(), api.RootID, "много")
	if err != nil {
		t.Fatal(err)
	}
	const n = 300
	for i := range n {
		upload(t, third, dir.ID, fmt.Sprintf("%03d.txt", i), strings.Repeat(fmt.Sprintf("%03d", i), 700))
	}
	h := c.client("Мак", nil)
	h.close()
	env := []string{"HELPER_ROOT=" + h.root, "HELPER_DB=" + h.dbPath, "HELPER_SERVER=" + c.srv.URL,
		"HELPER_USER=" + c.user.ID, "HELPER_TOKEN=" + c.srv.DeviceToken(t, c.user)}

	synced := func() []state.Base {
		db, err := state.Open(context.Background(), h.dbPath)
		if err != nil {
			return nil
		}
		defer db.Close()
		var bs []state.Base
		_ = db.View(context.Background(), func(tx *state.Tx) error {
			return tx.EachBase(func(b state.Base) error {
				if b.Kind == state.KindFile {
					bs = append(bs, b)
				}
				return nil
			})
		})
		return bs
	}
	// 100 KB/s for 600 KB: about 6 s of downloads to cut into.
	ch := startChild(t, append(env, "HELPER_LIMIT=102400"))
	waitFor(t, "some files synced", func() bool { return len(synced()) >= 50 })
	ch.kill(t)
	bs := synced()
	if len(bs) >= n {
		t.Fatalf("all %d files synced before the kill", len(bs))
	}
	for _, b := range bs {
		content, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(b.RelPath)))
		if err != nil {
			t.Fatalf("%s is synced in base but: %v", b.RelPath, err)
		}
		if shaOf(string(content)) != b.SHA256 {
			t.Fatalf("%s on disk isn't what base says", b.RelPath)
		}
	}
	t.Logf("killed with %d of %d files synced", len(bs), n)
	startChild(t, env).finish(t)
	if got := len(synced()); got != n {
		t.Fatalf("%d files synced after the second run, want %d", got, n)
	}
	h2 := &client{harness: &harness{t: t, root: h.root}}
	c.expectSynced(third, h2)
}
