package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusNotFound, CodeNotFound, "nothing here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body != (ErrorBody{Code: CodeNotFound, Message: "nothing here"}) {
		t.Fatalf("body = %+v", body)
	}
}

func TestLimitBody(t *testing.T) {
	var readErr error
	h := LimitBody(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name       string
		body       string
		chunked    bool
		wantStatus int
		wantMaxErr bool
	}{
		{"within limit", "12345678", false, http.StatusNoContent, false},
		{"declared too large", "123456789", false, http.StatusRequestEntityTooLarge, false},
		{"chunked too large", "123456789", true, http.StatusNoContent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readErr = nil
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if tc.chunked {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var maxErr *http.MaxBytesError
			if got := errors.As(readErr, &maxErr); got != tc.wantMaxErr {
				t.Fatalf("read err = %v, want MaxBytesError: %v", readErr, tc.wantMaxErr)
			}
			if tc.wantStatus == http.StatusRequestEntityTooLarge && !strings.Contains(rec.Body.String(), CodeBodyTooLarge) {
				t.Fatalf("body = %s", rec.Body)
			}
		})
	}
}
