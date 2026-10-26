package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	xslog "golang.org/x/exp/slog"
)

func TestStreamSeeks(t *testing.T) {
	const size = 3*blockSize + 12345
	full, err := io.ReadAll(newStream(7, size, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != size {
		t.Fatalf("%d bytes, want %d", len(full), size)
	}
	for _, off := range []int64{0, 1, blockSize - 1, blockSize, 2*blockSize + 77, size - 1, size} {
		got, err := io.ReadAll(newStream(7, size, off))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, full[off:]) {
			t.Fatalf("stream from %d differs from the full stream", off)
		}
	}
	var viaWriteTo bytes.Buffer
	if _, err := newStream(7, size, 0).WriteTo(&viaWriteTo); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaWriteTo.Bytes(), full) {
		t.Fatal("WriteTo differs from Read")
	}
	other, _ := io.ReadAll(newStream(8, blockSize, 0))
	if bytes.Equal(other, full[:blockSize]) {
		t.Fatal("seed doesn't change the content")
	}
}

// The upload survives a connection dropped mid-chunk: the client asks for
// the offset and the stored bytes are exactly the stream's.
func TestUploadResumesAfterBreak(t *testing.T) {
	dir := t.TempDir()
	composer := tusd.NewStoreComposer()
	filestore.New(dir).UseIn(composer)
	memorylocker.New().UseIn(composer)
	h, err := tusd.NewHandler(tusd.Config{
		BasePath:      "/api/v1/uploads/",
		StoreComposer: composer,
		Logger:        xslog.New(xslog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.StripPrefix("/api/v1/uploads", h))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)

	const size, chunk = 5*blockSize + 999, 2 * blockSize
	up := &uploader{c: newClient(base, "session", "x"), size: size, chunk: chunk, seed: 3, breakAt: size / 2}
	res, err := up.run(context.Background(), "f.bin", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed || len(res.Resumes) != 1 {
		t.Fatalf("resumes %+v, want one", res.Resumes)
	}
	if r := res.Resumes[0]; r.Sent != size/2 || r.Kept > r.Sent {
		t.Fatalf("resume %+v: sent should be the break point %d, kept no more than that", r, size/2)
	}
	if res.ResentBytes != res.Resumes[0].Sent-res.Resumes[0].Kept {
		t.Fatalf("re-sent %d, want what the server lost: %+v", res.ResentBytes, res.Resumes[0])
	}

	stored, err := os.ReadFile(filepath.Join(dir, path.Base(up.location)))
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.New()
	if _, err := newStream(3, size, 0).WriteTo(want); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(stored) != [32]byte(want.Sum(nil)) {
		t.Fatalf("stored %d bytes differ from the stream", len(stored))
	}
}

func TestBreakPointIsInsideAChunk(t *testing.T) {
	const chunk = 64 << 20
	for _, tc := range []struct {
		frac       float64
		size, want int64
	}{
		{0.5, 10 << 30, 5<<30 + chunk/2},
		{0.5, 3 * chunk, 3 * chunk / 2},
		{0.25, 1000, 250},
	} {
		if got := breakPoint(tc.frac, tc.size, chunk); got != tc.want {
			t.Errorf("breakPoint(%v, %d) = %d, want %d", tc.frac, tc.size, got, tc.want)
		}
	}
}

func TestByteSize(t *testing.T) {
	for in, want := range map[string]int64{"10GiB": 10 << 30, "64MiB": 64 << 20, "500MB": 500e6, "123": 123, "4GB": 4e9} {
		var b byteSize
		if err := b.Set(in); err != nil || int64(b) != want {
			t.Errorf("%q: %d, %v; want %d", in, b, err, want)
		}
	}
	var b byteSize
	if b.Set("lots") == nil {
		t.Error("accepted a non-size")
	}
}

func BenchmarkStream(b *testing.B) {
	b.SetBytes(1 << 30)
	for b.Loop() {
		if _, err := newStream(1, 1<<30, 0).WriteTo(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
