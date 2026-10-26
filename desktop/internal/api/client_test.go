package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fake serves handler and returns a client of it whose retries don't
// sleep, recording the delays they asked for.
func fake(t *testing.T, handler http.HandlerFunc, opts Options) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	if opts.Token == nil {
		opts.Token = StaticToken("cld_test")
	}
	c, err := New(srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	var delays []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return ctx.Err()
	}
	return c, &delays
}

func TestNormalizeServer(t *testing.T) {
	for in, want := range map[string]string{
		"https://Cloud.Example.com/":      "https://cloud.example.com",
		"https://cloud.example.com:443":   "https://cloud.example.com",
		"https://cloud.example.com:8443/": "https://cloud.example.com:8443",
		"http://localhost:8090":           "http://localhost:8090",
		"http://127.0.0.1:8090/":          "http://127.0.0.1:8090",
		"http://[::1]:8090":               "http://[::1]:8090",
		" https://example.com/cloud/ ":    "https://example.com/cloud",
	} {
		got, err := NormalizeServer(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"http://cloud.example.com", "ftp://x", "cloud.example.com", "https://u:p@x.com", "https://x.com/?a=1", "https://x.com/#f", "",
	} {
		if got, err := NormalizeServer(in); err == nil {
			t.Errorf("NormalizeServer(%q) = %q, want an error", in, got)
		}
	}
}

func TestHeaders(t *testing.T) {
	c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer cld_test" {
			http.Error(w, "auth "+got, http.StatusTeapot)
			return
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "cloud-desktop/1.2.3 (") {
			http.Error(w, "ua "+ua, http.StatusTeapot)
			return
		}
		_, _ = io.WriteString(w, `{"roots":[],"cursor":"c"}`)
	}, Options{ClientVersion: "1.2.3"})
	if _, err := c.Roots(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"code":"unauthorized","message":"sign in required"}`, ErrUnauthorized},
		{412, `{"code":"precondition_failed"}`, ErrPrecondition},
		{404, `{"code":"not_found"}`, ErrNotFound},
		{410, `{"code":"cursor_expired"}`, ErrCursorExpired},
		{413, `{"code":"quota_exceeded"}`, ErrQuota},
		{507, ``, ErrQuota},
		{423, `{"code":"locked"}`, ErrLocked},
		{409, `{"code":"name_conflict"}`, ErrNameConflict},
		{403, `{"code":"forbidden"}`, ErrForbidden},
		{400, `{"code":"invalid_grant"}`, ErrInvalidGrant},
	} {
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}, Options{})
		_, err := c.Roots(context.Background())
		var ae *APIError
		if !errors.Is(err, tc.want) || !errors.As(err, &ae) || ae.Status != tc.status {
			t.Errorf("%d %s: %v, want %v", tc.status, tc.body, err, tc.want)
		}
	}
}

func TestRetry(t *testing.T) {
	t.Run("GET after 503 and 429, with Retry-After", func(t *testing.T) {
		var n atomic.Int32
		c, delays := fake(t, func(w http.ResponseWriter, r *http.Request) {
			switch n.Add(1) {
			case 1:
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusServiceUnavailable)
			case 2:
				w.WriteHeader(http.StatusTooManyRequests)
			default:
				_, _ = io.WriteString(w, `{"roots":[],"cursor":"c"}`)
			}
		}, Options{Retry: RetryPolicy{Attempts: 5, MinDelay: time.Second, MaxDelay: 10 * time.Second}})
		if _, err := c.Roots(context.Background()); err != nil {
			t.Fatal(err)
		}
		d := *delays
		if n.Load() != 3 || len(d) != 2 || d[0] != 7*time.Second || d[1] < time.Second || d[1] > 2*time.Second {
			t.Errorf("%d requests, delays %v", n.Load(), d)
		}
	})
	t.Run("gives up after Attempts", func(t *testing.T) {
		var n atomic.Int32
		c, delays := fake(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		}, Options{Retry: RetryPolicy{Attempts: 4, MinDelay: time.Second, MaxDelay: 3 * time.Second}})
		_, err := c.Roots(context.Background())
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadGateway || n.Load() != 4 {
			t.Fatalf("%v after %d requests", err, n.Load())
		}
		// Exponential, jittered within [d/2, d], capped at MaxDelay.
		for i, d := range *delays {
			hi := min(time.Second<<i, 3*time.Second)
			if d < hi/2 || d > hi {
				t.Errorf("delay %d = %v, want within [%v, %v]", i, d, hi/2, hi)
			}
		}
	})
	t.Run("no retry of 4xx", func(t *testing.T) {
		var n atomic.Int32
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		}, Options{})
		if _, err := c.Roots(context.Background()); !errors.Is(err, ErrUnauthorized) || n.Load() != 1 {
			t.Errorf("%v after %d requests", err, n.Load())
		}
	})
	t.Run("POST not repeated after a 500", func(t *testing.T) {
		var n atomic.Int32
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}, Options{})
		if _, err := c.Mkdir(context.Background(), RootID, "x"); err == nil || n.Load() != 1 {
			t.Errorf("%v after %d requests", err, n.Load())
		}
	})
	t.Run("POST repeated after a 429", func(t *testing.T) {
		var n atomic.Int32
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"name":"x"}` {
				http.Error(w, "body "+string(body), http.StatusTeapot)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"n1","parentId":"p","name":"x","kind":"dir"}`)
		}, Options{})
		n1, err := c.Mkdir(context.Background(), RootID, "x")
		if err != nil || n1.ID != "n1" || n1.ParentID != "p" || n.Load() != 2 {
			t.Errorf("%+v, %v after %d requests", n1, err, n.Load())
		}
	})
	t.Run("network error", func(t *testing.T) {
		var n atomic.Int32
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				// Headers promise a body that never comes.
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"roots":`)
				return
			}
			_, _ = io.WriteString(w, `{"roots":[],"cursor":"c"}`)
		}, Options{})
		if _, err := c.Roots(context.Background()); err != nil || n.Load() != 2 {
			t.Errorf("%v after %d requests", err, n.Load())
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
			cancel()
			w.WriteHeader(http.StatusServiceUnavailable)
		}, Options{})
		if _, err := c.Roots(ctx); !errors.Is(err, context.Canceled) {
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Errorf("err = %v, want the 503 or context.Canceled", err)
			}
		}
	})
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"":                              0,
		"5":                             5 * time.Second,
		"-1":                            0,
		"Thu, 01 Jan 2026 00:00:30 GMT": 30 * time.Second,
		"Wed, 31 Dec 2025 00:00:00 GMT": 0,
		"soon":                          0,
	} {
		if got := retryAfter(in, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestInfoCompatibility(t *testing.T) {
	for _, tc := range []struct {
		body    string
		version string
		want    error
	}{
		{`{"apiVersion":1,"minClientVersion":"0.0.0","maxWait":55}`, "0.0.0-dev", nil},
		{`{"apiVersion":1,"minClientVersion":"1.2.0"}`, "1.2.0-rc.1", nil},
		{`{"apiVersion":1,"minClientVersion":"1.10.0"}`, "1.9.9", ErrClientTooOld},
		{`{"apiVersion":2,"minClientVersion":"0.0.0"}`, "1.0.0", ErrIncompatible},
	} {
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/sync/info" || r.Header.Get("Authorization") != "" {
				http.Error(w, "unexpected", http.StatusTeapot)
				return
			}
			_, _ = io.WriteString(w, tc.body)
		}, Options{ClientVersion: tc.version})
		info, err := c.Info(context.Background())
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s with %s: %v, want %v", tc.body, tc.version, err, tc.want)
		}
		if info.APIVersion == 0 {
			t.Errorf("%s: info not filled in on %v", tc.body, err)
		}
	}
}

func TestTree(t *testing.T) {
	const head = `{"cursor":"c1"}` + "\n" + `{"id":"r","parentId":null,"name":"","kind":"dir"}` + "\n"
	for name, tc := range map[string]struct {
		body  string
		abort bool
		want  error
		nodes int
	}{
		"whole":                  {body: head + `{"id":"a","parentId":"r","name":"a","kind":"file"}` + "\n", nodes: 2},
		"cut between lines":      {body: head, abort: true, want: ErrTruncated, nodes: 1},
		"cut midline":            {body: head + `{"id":"a","par`, want: ErrTruncated, nodes: 1},
		"no root":                {body: `{"cursor":"c1"}` + "\n", want: ErrTruncated},
		"empty":                  {body: "", want: ErrTruncated},
		"aborted before nothing": {abort: true, want: ErrTruncated},
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Query().Get("root") != "root" {
					http.Error(w, "root", http.StatusTeapot)
					return
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
				w.(http.Flusher).Flush()
				if tc.abort {
					panic(http.ErrAbortHandler)
				}
			}, Options{Retry: RetryPolicy{Attempts: 3, MinDelay: time.Millisecond, MaxDelay: time.Millisecond}})
			var got []Node
			cursor, err := c.Tree(context.Background(), RootID, func(n Node) error {
				got = append(got, n)
				return nil
			})
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && cursor != "c1" {
				t.Errorf("cursor = %q", cursor)
			}
			// Nodes handed out once: a stream that got anywhere isn't
			// retried, one that got nowhere is.
			if len(got) != tc.nodes {
				t.Errorf("got %d nodes, want %d", len(got), tc.nodes)
			}
			if wantReq := int32(1); tc.want != nil && tc.nodes == 0 {
				if requests.Load() != 3 {
					t.Errorf("%d requests for a stream with nothing in it, want 3", requests.Load())
				}
			} else if requests.Load() != wantReq {
				t.Errorf("%d requests, want %d", requests.Load(), wantReq)
			}
		})
	}

	t.Run("fn's error", func(t *testing.T) {
		c, _ := fake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, head) }, Options{})
		stop := errors.New("stop")
		if _, err := c.Tree(context.Background(), RootID, func(Node) error { return stop }); err != stop {
			t.Errorf("err = %v, want fn's own", err)
		}
	})
}

func TestUploadMetadata(t *testing.T) {
	decode := func(s string) map[string]string {
		m := map[string]string{}
		for _, kv := range strings.Split(s, ",") {
			k, v, _ := strings.Cut(kv, " ")
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatal(err)
			}
			m[k] = string(b)
		}
		return m
	}
	mtime := time.Date(2025, 1, 2, 3, 4, 5, 600, time.FixedZone("x", 3600))
	got := decode((&tusUpload{u: Upload{ParentID: "p", Name: "Отчёт, итог.txt", OnConflict: ConflictFail, MTime: mtime}}).metadata())
	if fmt.Sprint(got) != fmt.Sprint(map[string]string{"filename": "Отчёт, итог.txt", "parentId": "p", "onConflict": "fail", "mtime": "2025-01-02T02:04:05.0000006Z"}) {
		t.Errorf("new file: %v", got)
	}
	got = decode((&tusUpload{u: Upload{NodeID: "n", IfMatch: "e", Name: "ignored", SHA256: "ab"}}).metadata())
	if fmt.Sprint(got) != fmt.Sprint(map[string]string{"nodeId": "n", "ifMatch": "e", "sha256": "ab"}) {
		t.Errorf("new version: %v", got)
	}
}

func TestContentRange(t *testing.T) {
	for in, want := range map[string][3]int64{
		"bytes 10-99/100": {10, 100, 1},
		"bytes */100":     {0, 100, 1},
		"bytes 10-99":     {0, 0, 0},
		"items 1-2/3":     {0, 0, 0},
	} {
		start, size, ok := contentRange(in)
		if ok != (want[2] == 1) || ok && (start != want[0] || size != want[1]) {
			t.Errorf("contentRange(%q) = %d, %d, %v", in, start, size, ok)
		}
	}
}

func TestLimiter(t *testing.T) {
	if NewLimiter(0) != nil {
		t.Error("no limit should be nil")
	}
	lim := NewLimiter(256 << 10)
	start := time.Now()
	// The first burst (64 KiB) is free; the rest, 192 KiB, takes ~0.75 s.
	n, err := io.Copy(io.Discard, limitReader(context.Background(), strings.NewReader(strings.Repeat("x", 256<<10)), lim))
	if err != nil || n != 256<<10 {
		t.Fatal(n, err)
	}
	if d := time.Since(start); d < 600*time.Millisecond || d > 3*time.Second {
		t.Errorf("256 KiB at 256 KiB/s took %v", d)
	}
}

// Ping is one request, without retries or a token: an offline client
// asks it cheaply until the server answers.
func TestPing(t *testing.T) {
	var calls atomic.Int64
	status := atomic.Int64{}
	status.Store(http.StatusServiceUnavailable)
	c, delays := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/sync/info" || r.Header.Get("Authorization") != "" {
			t.Errorf("%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(int(status.Load()))
	}, Options{})
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("ping of a 503 succeeded")
	}
	if calls.Load() != 1 || len(*delays) != 0 {
		t.Fatalf("%d calls, delays %v: retried", calls.Load(), *delays)
	}
	status.Store(http.StatusOK)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}
