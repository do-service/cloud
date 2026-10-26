package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Errors a caller branches on. An *APIError matches them with errors.Is,
// by status and, where a status means several things, by code.
var (
	// ErrUnauthorized: the token is missing, revoked or the account is
	// blocked. The connection needs signing in again; retrying won't help.
	ErrUnauthorized = errors.New("api: not signed in (the device token was revoked or is missing)")
	// ErrPrecondition: a conditional write (If-Match, tus ifMatch) found
	// the node changed since the ETag the caller based it on.
	ErrPrecondition = errors.New("api: precondition failed (changed on the server)")
	ErrNotFound     = errors.New("api: not found")
	// ErrCursorExpired: the change journal no longer reaches back to the
	// cursor; resync in full.
	ErrCursorExpired = errors.New("api: sync cursor expired")
	// ErrQuota: not enough quota left (413 quota_exceeded or 507).
	ErrQuota = errors.New("api: quota exceeded")
	// ErrLocked: locked by an editor or a WebDAV client; retry later.
	ErrLocked = errors.New("api: locked")
	// ErrNameConflict: the name is taken in that folder.
	ErrNameConflict = errors.New("api: name conflict")
	ErrForbidden    = errors.New("api: forbidden")
	// ErrInvalidGrant: the sign-in code was refused (used, expired, or
	// the verifier doesn't match); start the sign-in over.
	ErrInvalidGrant = errors.New("api: sign-in code refused")
)

// Error codes of the server (backend/internal/httpx Code*) the client
// tells apart.
const (
	CodeUnauthorized       = "unauthorized"
	CodeNotFound           = "not_found"
	CodeCursorExpired      = "cursor_expired"
	CodePreconditionFailed = "precondition_failed"
	CodeQuotaExceeded      = "quota_exceeded"
	CodeLocked             = "locked"
	CodeNameConflict       = "name_conflict"
	CodeForbidden          = "forbidden"
	CodeInvalidGrant       = "invalid_grant"
	CodeChecksumMismatch   = "checksum_mismatch"
)

// APIError is an error response of the server: its status and the
// {"code","message"} body, when it had one.
type APIError struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is the server's Retry-After, zero if it sent none.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("api: %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("api: %d %s: %s", e.Status, e.Code, e.Message)
}

func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrPrecondition:
		return e.Status == http.StatusPreconditionFailed
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrCursorExpired:
		return e.Status == http.StatusGone && e.Code == CodeCursorExpired
	case ErrQuota:
		return e.Status == http.StatusInsufficientStorage || e.Code == CodeQuotaExceeded
	case ErrLocked:
		return e.Status == http.StatusLocked
	case ErrNameConflict:
		return e.Code == CodeNameConflict
	case ErrForbidden:
		return e.Status == http.StatusForbidden
	case ErrInvalidGrant:
		return e.Code == CodeInvalidGrant
	case ErrChecksum:
		// An upload whose content didn't match its sha256 or size.
		return e.Code == CodeChecksumMismatch
	}
	return false
}

// maxErrorBody bounds what is read of an error response: a proxy's HTML
// error page is no use beyond its status.
const maxErrorBody = 64 << 10

// readError turns a non-2xx response into an *APIError, consuming and
// closing the body.
func readError(res *http.Response) error {
	defer res.Body.Close()
	e := &APIError{Status: res.StatusCode, RetryAfter: retryAfter(res.Header.Get("Retry-After"), time.Now())}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil {
		e.Code, e.Message = body.Code, body.Message
	}
	return e
}

// retryAfter parses Retry-After: seconds or an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
