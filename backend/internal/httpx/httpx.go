// Package httpx holds the JSON response helpers shared by every HTTP-facing
// package, so error bodies look the same whichever package wrote them.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error codes are a stable, machine-readable contract: the frontend maps
// them to localized text, so renaming one is a breaking API change. Message
// is only an English hint for developers and may change freely.
const (
	CodeInternal           = "internal_error"
	CodeNotFound           = "not_found"
	CodeMethodNotAllowed   = "method_not_allowed"
	CodeBodyTooLarge       = "request_body_too_large"
	CodeDatabaseNotReady   = "database_unavailable"
	CodeDataDirNotReady    = "data_dir_unavailable"
	CodeFrontendNotBuilt   = "frontend_not_built"
	CodeUnauthorized       = "unauthorized"
	CodeCrossOrigin        = "cross_origin_request"
	CodeInvalidRequest     = "invalid_request"
	CodeInvalidCredentials = "invalid_credentials"
	CodeTooManyRequests    = "too_many_requests"
	// OIDC sign-in failures, passed to the login page as ?error=.
	CodeProviderUnavailable = "provider_unavailable"
	CodeOIDCFailed          = "oidc_failed"
	CodeSignupDisabled      = "signup_disabled"
	CodeAccountDisabled     = "account_disabled"
	CodeAppPasswordLimit    = "app_password_limit"
	CodeDeviceLimit         = "device_limit"
	CodeDeviceForbidden     = "device_forbidden"
	// CodeInvalidGrant: a desktop sign-in code that is unknown, used,
	// expired or doesn't match its verifier (RFC 6749's name for it).
	CodeInvalidGrant       = "invalid_grant"
	CodeNotADirectory      = "not_a_directory"
	CodeNotAFile           = "not_a_file"
	CodeNameConflict       = "name_conflict"
	CodeInvalidName        = "invalid_name"
	CodeMoveIntoItself     = "move_into_itself"
	CodeRootImmutable      = "root_immutable"
	CodeQueryTooShort      = "query_too_short"
	CodeQuotaExceeded      = "quota_exceeded"
	CodeChecksumMismatch   = "checksum_mismatch"
	CodeForbidden          = "forbidden"
	CodeCrossOwnerMove     = "cross_owner_move"
	CodeInvalidGrantee     = "invalid_grantee"
	CodeInvalidPermissions = "invalid_permissions"
	CodeLinksDisabled      = "links_disabled"
	CodeLocked             = "locked"
	CodeEditorUnavailable  = "editor_unavailable"
	CodeNotEditable        = "not_editable"
	CodeUsernameTaken      = "username_taken"
	CodeInvalidUsername    = "invalid_username"
	CodeWeakPassword       = "weak_password"
	CodeLastAdmin          = "last_admin"
	CodeOwnAccount         = "own_account"
	// CodeCursorExpired: the sync journal no longer reaches back to the
	// cursor, the client has to resync in full.
	CodeCursorExpired = "cursor_expired"
	// CodePreconditionFailed: a conditional write (If-Match, a tus
	// upload's ifMatch) found the node changed since the client saw it.
	CodePreconditionFailed = "precondition_failed"
)

// ErrorBody is the JSON shape of every error response.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already out; all that's left is to note it.
		slog.Debug("write json response", "err", err)
	}
}

// WriteError writes {"code": code, "message": message} with the given status.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Code: code, Message: message})
}

// LimitBody caps request bodies at n bytes. Meant for JSON routes only and
// applied per route, never globally: file uploads are unbounded by design.
// A declared Content-Length over n is refused up front with 413; a chunked
// body that runs over makes the handler's own read fail with
// *http.MaxBytesError.
func LimitBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > n {
				WriteError(w, http.StatusRequestEntityTooLarge, CodeBodyTooLarge, "request body exceeds the limit for this endpoint")
				return
			}
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}
