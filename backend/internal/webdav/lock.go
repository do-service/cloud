// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// LOCK and UNLOCK started from golang.org/x/net/webdav v0.61.0
// (webdav.go: handleLock, handleUnlock; lock.go: parseTimeout); the locks
// themselves are fs's, in Postgres.

package webdav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/do-service/cloud/backend/internal/fs"
)

var (
	errInvalidIfHeader  = errors.New("webdav: invalid If header")
	errInvalidLockToken = errors.New("webdav: invalid lock token")
	errInvalidTimeout   = errors.New("webdav: invalid timeout")
	errIfFailed         = errors.New("webdav: no If header state list holds")
)

// infiniteTimeout is a Timeout of Infinite; fs caps it.
const infiniteTimeout = -1

// conditions evaluates the request's If header (RFC 4918 §10.4) — t is
// the request's own resource, which untagged lists are about — and
// returns the request context with the lock tokens it presents. Whether
// those tokens unlock what the request changes is fs's to check when
// writing; here a state list holds when each of its conditions does: a
// token names a lock covering the resource, an ETag is the resource's.
func (h *Handler) conditions(r *http.Request, t target) (context.Context, int, error) {
	ctx := r.Context()
	hdr := r.Header.Get("If")
	if hdr == "" {
		return ctx, 0, nil
	}
	ih, ok := parseIfHeader(hdr)
	if !ok {
		return nil, http.StatusBadRequest, errInvalidIfHeader
	}
	type state struct {
		exists bool
		etag   string
		tokens []string
	}
	states := map[string]state{}
	stateOf := func(t target) (state, error) {
		if st, ok := states[t.key()]; ok {
			return st, nil
		}
		var st state
		n, err := h.lookup(ctx, t, t.names)
		switch {
		case errors.Is(err, fs.ErrNotFound):
		case err != nil:
			return state{}, err
		default:
			st.exists, st.etag = true, quote(n.ETag)
			// The virtual folder of shares has no id and holds no locks.
			if n.ID == "" {
				break
			}
			locks, err := h.files.Locks(ctx, t.user.ID, n.ID, false)
			if err != nil {
				return state{}, err
			}
			for _, l := range locks[n.ID] {
				st.tokens = append(st.tokens, l.Token)
			}
		}
		states[t.key()] = st
		return st, nil
	}

	var tokens []string
	holds := false
	for _, l := range ih.lists {
		// Every token in the header counts as submitted, whichever list
		// it is in (§10.4.1).
		for _, c := range l.conditions {
			if c.Token != "" && !c.Not {
				tokens = append(tokens, c.Token)
			}
		}
		if holds {
			continue
		}
		res := t
		if l.resourceTag != "" {
			u, err := url.Parse(l.resourceTag)
			if err != nil || u.Host != "" && u.Host != r.Host {
				continue
			}
			if res, _, err = h.resolve(ctx, u.Path); err != nil {
				continue
			}
		}
		st, err := stateOf(res)
		if err != nil {
			status, err := fail(err, http.StatusConflict)
			return nil, status, err
		}
		holds = true
		for _, c := range l.conditions {
			var match bool
			if c.Token != "" {
				match = slices.Contains(st.tokens, c.Token)
			} else {
				match = st.exists && c.ETag == st.etag
			}
			if match == c.Not {
				holds = false
				break
			}
		}
	}
	if !holds {
		// "If this header is evaluated and all state lists fail, then the
		// request must fail with a 412 (Precondition Failed) status."
		return nil, http.StatusPreconditionFailed, errIfFailed
	}
	return fs.WithLockTokens(ctx, tokens), 0, nil
}

func (h *Handler) lock(w http.ResponseWriter, r *http.Request) (int, error) {
	duration, err := parseTimeout(r.Header.Get("Timeout"))
	if err != nil {
		return http.StatusBadRequest, err
	}
	li, refresh, status, err := readLockInfo(r.Body)
	if err != nil {
		return status, err
	}
	t, status, err := h.resolve(r.Context(), r.URL.Path)
	if err != nil {
		return status, err
	}

	var l fs.Lock
	created := false
	if refresh {
		// An empty body refreshes the lock the If header names.
		ih, ok := parseIfHeader(r.Header.Get("If"))
		if !ok {
			return http.StatusBadRequest, errInvalidIfHeader
		}
		token := ""
		if len(ih.lists) == 1 && len(ih.lists[0].conditions) == 1 {
			token = ih.lists[0].conditions[0].Token
		}
		if token == "" {
			return http.StatusBadRequest, errInvalidLockToken
		}
		if l, err = h.files.RefreshLock(r.Context(), t.user.ID, token, duration); err != nil {
			if errors.Is(err, fs.ErrNoLock) {
				return http.StatusPreconditionFailed, err
			}
			return fail(err, http.StatusConflict)
		}
	} else {
		// No Depth means infinity; nothing but 0 or infinity is allowed
		// (§9.10.3).
		depth := r.Header.Get("Depth")
		if depth != "" && depth != "0" && depth != "infinity" {
			return http.StatusBadRequest, errInvalidDepth
		}
		if t.virtual && len(t.names) == 1 {
			return http.StatusForbidden, fs.ErrForbidden
		}
		ctx, status, err := h.conditions(r, t)
		if err != nil {
			return status, err
		}
		n, err := h.lookup(ctx, t, t.names)
		switch {
		// Locking a name that doesn't exist creates an empty file there
		// (§9.10.4): how clients reserve a name before writing.
		case errors.Is(err, fs.ErrNotFound) && !t.isRoot():
			dir, status, err := h.parentDir(ctx, t)
			if err != nil {
				return status, err
			}
			if n, _, err = h.files.PutFile(ctx, t.user.ID, dir.ID, t.name(), strings.NewReader(""), 0, fs.PutCond{IfNoneMatch: []string{"*"}}); err != nil {
				return fail(err, http.StatusConflict)
			}
			created = true
		case err != nil:
			return fail(err, http.StatusConflict)
		}
		l, err = h.files.Lock(ctx, t.user.ID, n.ID, fs.LockRequest{
			Kind: fs.LockWebDAV, Deep: depth != "0", OwnerInfo: li.Owner.InnerXML, Timeout: duration,
		})
		if err != nil {
			return fail(err, http.StatusConflict)
		}
		// The Lock-Token value is a Coded-URL: in angle brackets.
		w.Header().Set("Lock-Token", "<"+l.Token+">")
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_, _ = fmt.Fprint(w, xml.Header+`<D:prop xmlns:D="DAV:"><D:lockdiscovery>`+activeLock(l, r.URL.EscapedPath())+`</D:lockdiscovery></D:prop>`)
	return 0, nil
}

// activeLock is a lock as lockdiscovery shows it; root is the escaped
// href of the node it is on.
func activeLock(l fs.Lock, root string) string {
	depth := "0"
	if l.Deep {
		depth = "infinity"
	}
	owner := ""
	if l.OwnerInfo != "" {
		owner = "<D:owner>" + l.OwnerInfo + "</D:owner>"
	}
	seconds := int64(math.Ceil(time.Until(l.ExpiresAt).Seconds()))
	return `<D:activelock><D:locktype><D:write/></D:locktype><D:lockscope><D:exclusive/></D:lockscope>` +
		`<D:depth>` + depth + `</D:depth>` + owner + `<D:timeout>Second-` + strconv.FormatInt(max(seconds, 0), 10) + `</D:timeout>` +
		`<D:locktoken><D:href>` + escapeString(l.Token) + `</D:href></D:locktoken>` +
		`<D:lockroot><D:href>` + escapeString(root) + `</D:href></D:lockroot></D:activelock>`
}

func (h *Handler) unlock(w http.ResponseWriter, r *http.Request) (int, error) {
	t, status, err := h.resolve(r.Context(), r.URL.Path)
	if err != nil {
		return status, err
	}
	// The Lock-Token value is a Coded-URL: in angle brackets.
	token := r.Header.Get("Lock-Token")
	if len(token) < 2 || token[0] != '<' || token[len(token)-1] != '>' {
		return http.StatusBadRequest, errInvalidLockToken
	}
	switch err := h.files.Unlock(r.Context(), t.user.ID, token[1:len(token)-1]); {
	case err == nil:
		return http.StatusNoContent, nil
	case errors.Is(err, fs.ErrNoLock):
		return http.StatusConflict, err
	default:
		return fail(err, http.StatusConflict)
	}
}

// parseTimeout reads the Timeout header of a LOCK (§10.7): the first of
// its choices, "Infinite" or "Second-n".
func parseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return infiniteTimeout, nil
	}
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "Infinite" {
		return infiniteTimeout, nil
	}
	const pre = "Second-"
	if !strings.HasPrefix(s, pre) {
		return 0, errInvalidTimeout
	}
	s = s[len(pre):]
	if s == "" || s[0] < '0' || '9' < s[0] {
		return 0, errInvalidTimeout
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || 1<<32-1 < n {
		return 0, errInvalidTimeout
	}
	return time.Duration(n) * time.Second, nil
}
