package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sidecar is the path of the running hash of the upload at url.
func (e env) sidecar(url string) string {
	return filepath.Join(e.dataDir, "uploads", "tus", filepath.Base(url)+sumSuffix)
}

// finishes sends the last chunk and checks the file it became has the
// content's hash, read back from fs as the node's.
func (e env) finishes(t *testing.T, url string, offset int, content string) {
	t.Helper()
	resp := e.patch(t, "alice", url, offset, content[offset:])
	nodeID := resp.Header.Get(NodeIDHeader)
	if resp.StatusCode != http.StatusNoContent || nodeID == "" {
		t.Fatalf("last chunk: %d %v", resp.StatusCode, resp.Header)
	}
	n, err := e.files.Get(context.Background(), e.users["alice"], nodeID)
	sum := sha256.Sum256([]byte(content))
	if err != nil || !bytes.Equal(n.ContentHash, sum[:]) {
		t.Fatalf("node: %+v, %v; want hash %x", n, err, sum)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dataDir, "uploads", "tus")); len(left) != 0 {
		t.Fatalf("%d files left in uploads/tus", len(left))
	}
}

func TestUploadHashedWhileWritten(t *testing.T) {
	e := newEnv(t)
	content := strings.Repeat("abcdefghij", 3000)
	_, url := e.create(t, len(content), http.StatusCreated, "filename", "a.txt")
	for _, off := range []int{0, 10000} {
		if resp := e.patch(t, "alice", url, off, content[off:off+10000]); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("chunk at %d: %d", off, resp.StatusCode)
		}
	}
	h, err := newHashStore(filepath.Join(e.dataDir, "uploads", "tus")).load(filepath.Base(url), 20000)
	if err != nil {
		t.Fatalf("sidecar after two chunks: %v", err)
	}
	if want := sha256.Sum256([]byte(content[:20000])); !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatalf("running hash %x, want %x", h.Sum(nil), want)
	}
	e.finishes(t, url, 20000, content)
	if n := e.rereads.Load(); n != 0 {
		t.Fatalf("finishing reread the upload %d times", n)
	}
}

// A connection cut mid-chunk keeps what arrived, and the hash with it.
func TestUploadHashAfterCutChunk(t *testing.T) {
	e := newEnv(t)
	content := strings.Repeat("0123456789", 1000)
	sum := sha256.Sum256([]byte(content))
	_, loc := e.create(t, len(content), http.StatusCreated, "filename", "a.txt", "sha256", hex.EncodeToString(sum[:]))
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "PATCH %s HTTP/1.1\r\nHost: %s\r\nTus-Resumable: 1.0.0\r\nContent-Type: application/offset+octet-stream\r\nUpload-Offset: 0\r\nContent-Length: %d\r\nCookie: session=%s\r\n\r\n%s",
		u.Path, u.Host, len(content), e.cookies["alice"], content[:3000])
	conn.Close()

	var offset string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if offset = e.do(t, "alice", "HEAD", loc, nil, "").Header.Get("Upload-Offset"); offset == "3000" {
			break
		}
	}
	if offset != "3000" {
		t.Fatalf("offset after the cut: %q", offset)
	}
	e.finishes(t, loc, 3000, content)
	if n := e.rereads.Load(); n != 0 {
		t.Fatalf("finishing reread the upload %d times", n)
	}
}

// A sidecar that doesn't match the data is dropped, and finishing hashes
// the file itself.
func TestUploadHashFallback(t *testing.T) {
	content := strings.Repeat("abcdefghij", 3000)
	for name, spoil := range map[string]func(t *testing.T, e env, url string, first []byte){
		"deleted": func(t *testing.T, e env, url string, _ []byte) {
			if err := os.Remove(e.sidecar(url)); err != nil {
				t.Fatal(err)
			}
		},
		// As if the server died between writing a chunk and saving the hash.
		"behind": func(t *testing.T, e env, url string, first []byte) {
			if err := os.WriteFile(e.sidecar(url), first, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"before a reboot": func(t *testing.T, e env, url string, _ []byte) {
			raw, err := os.ReadFile(e.sidecar(url))
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte(bootID()), []byte("another-boot"), 1)
			if err := os.WriteFile(e.sidecar(url), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			sum := sha256.Sum256([]byte(content))
			_, url := e.create(t, len(content), http.StatusCreated, "filename", "a.txt", "sha256", hex.EncodeToString(sum[:]))
			e.patch(t, "alice", url, 0, content[:10000])
			first, err := os.ReadFile(e.sidecar(url))
			if err != nil {
				t.Fatal(err)
			}
			e.patch(t, "alice", url, 10000, content[10000:20000])
			spoil(t, e, url, first)
			e.finishes(t, url, 20000, content)
			if n := e.rereads.Load(); n != 1 {
				t.Fatalf("finish read the upload %d times, want 1", n)
			}
		})
	}
}

func TestUploadHashedChecksumMismatch(t *testing.T) {
	e := newEnv(t)
	sum := sha256.Sum256([]byte("other content"))
	_, url := e.create(t, 10, http.StatusCreated, "filename", "a.txt", "sha256", hex.EncodeToString(sum[:]))
	e.patch(t, "alice", url, 0, "01234")
	resp := e.patch(t, "alice", url, 5, "56789")
	if resp.StatusCode != http.StatusConflict || errorCode(t, resp) != "checksum_mismatch" {
		t.Fatalf("finish: %d", resp.StatusCode)
	}
	if n := e.rereads.Load(); n != 0 {
		t.Fatalf("finishing reread the upload %d times", n)
	}
	if _, err := os.Stat(e.sidecar(url)); !os.IsNotExist(err) || e.reserved(t) != 0 {
		t.Fatalf("sidecar: %v, reservations %d", err, e.reserved(t))
	}
}

// DELETE removes the sidecar with the upload.
func TestUploadHashTerminate(t *testing.T) {
	e := newEnv(t)
	_, url := e.create(t, 10, http.StatusCreated, "filename", "a.txt")
	e.patch(t, "alice", url, 0, "01234")
	if _, err := os.Stat(e.sidecar(url)); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(t, "alice", "DELETE", url, nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dataDir, "uploads", "tus")); len(left) != 0 {
		t.Fatalf("%d files left in uploads/tus", len(left))
	}
}

func TestUploadIDOf(t *testing.T) {
	for name, want := range map[string]string{
		"x":            "x",
		"x.info":       "x",
		"x.sha256":     "x",
		"x.sha256.tmp": "x",
	} {
		if got := uploadIDOf(name); got != want {
			t.Errorf("uploadIDOf(%q) = %q, want %q", name, got, want)
		}
	}
}
