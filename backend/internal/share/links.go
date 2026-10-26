package share

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
)

// Public links (R10): a URL with a token gives anyone who has it the
// link's mode on its node's subtree. The rights themselves are applied by
// access, for the principal access.LinkPrincipal(link id).

var (
	ErrNoLink = errors.New("share: no such link")
	// ErrLinksDisabled: tokens are kept sealed with SECRET_KEY, so
	// without one no link can be made.
	ErrLinksDisabled = errors.New("share: public links need SECRET_KEY")
	ErrInvalidMode   = errors.New("share: mode must be view, download or upload; upload is for folders")
	ErrBadPassword   = errors.New("share: wrong link password")
	ErrPasswordLen   = errors.New("share: link password must be 1 to 256 characters")
)

const (
	ModeView     = "view"
	ModeDownload = "download"
	ModeUpload   = "upload"
)

// LinkSessionTTL is how long a browser that gave a link's password may use
// it before asking again.
const LinkSessionTTL = 12 * time.Hour

const maxLinkPassword = 256

type Link struct {
	ID     string
	NodeID string
	// Token is what the URL carries: /s/{token}. It is empty for a link
	// sealed under an earlier SECRET_KEY: the link still works for whoever
	// has its URL, so it is listed to be revoked, but can't be copied.
	Token       string
	Mode        string
	HasPassword bool
	ExpiresAt   *time.Time
	CreatedAt   time.Time
}

// LinkChange is an edit to a link; nil fields stay as they are.
type LinkChange struct {
	Mode *string
	// Password: "" removes it.
	Password  *string
	ExpiresAt **time.Time
}

// Links manages public links.
type Links struct {
	svc *Service
	box *auth.SecretBox
}

func NewLinks(s *Service, box *auth.SecretBox) *Links { return &Links{svc: s, box: box} }

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// sealContext binds a sealed token to links, so one can't be passed off
// as another kind of secret.
const sealContext = "link"

func validMode(mode string, n fs.Node) bool {
	switch mode {
	case ModeView, ModeDownload:
		return true
	case ModeUpload:
		// A file drop puts files in a folder, and needs write to.
		return n.Kind == fs.KindDir && n.Perms.Has(access.Write)
	}
	return false
}

func (l *Links) hashPassword(ctx context.Context, password string) (*string, error) {
	if password == "" {
		return nil, nil
	}
	if len([]rune(password)) > maxLinkPassword {
		return nil, ErrPasswordLen
	}
	h, err := auth.HashPassword(ctx, password)
	return &h, err
}

// Create makes a link to the node, by its owner or a resharer.
func (l *Links) Create(ctx context.Context, actorID, nodeID, mode, password string, expiresAt *time.Time) (Link, error) {
	n, err := l.svc.sharable(ctx, actorID, nodeID)
	if err != nil {
		return Link{}, err
	}
	if !validMode(mode, n) {
		return Link{}, ErrInvalidMode
	}
	pw, err := l.hashPassword(ctx, password)
	if err != nil {
		return Link{}, err
	}
	var raw [32]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(raw[:])
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	sealed, err := l.box.Seal(token, sealContext)
	if errors.Is(err, auth.ErrNoSecretKey) {
		return Link{}, ErrLinksDisabled
	}
	if err != nil {
		return Link{}, err
	}
	link := Link{NodeID: n.ID, Token: token, Mode: mode, HasPassword: pw != nil, ExpiresAt: expiresAt}
	err = l.svc.db.QueryRow(ctx, `
		INSERT INTO links (node_id, token_hash, token_enc, mode, password_hash, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text, created_at`,
		n.ID, tokenHash(token), sealed, mode, pw, expiresAt, actorID).Scan(&link.ID, &link.CreatedAt)
	if err != nil {
		return Link{}, fmt.Errorf("create link: %w", err)
	}
	return link, nil
}

// List returns the node's links, tokens unsealed for copying.
func (l *Links) List(ctx context.Context, actorID, nodeID string) ([]Link, error) {
	n, err := l.svc.sharable(ctx, actorID, nodeID)
	if err != nil {
		return nil, err
	}
	rows, err := l.svc.db.Query(ctx, `
		SELECT id::text, node_id::text, token_enc, mode, password_hash IS NOT NULL, expires_at, created_at
		FROM links WHERE node_id = $1 ORDER BY created_at, id`, n.ID)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Link, error) { return l.scan(row) })
	if err != nil {
		return nil, err
	}
	// One line per listing, not per link: a key change leaves every old
	// link like this.
	sealed := 0
	for _, link := range list {
		if link.Token == "" {
			sealed++
		}
	}
	if sealed > 0 {
		slog.WarnContext(ctx, "share: link tokens can't be opened with the current SECRET_KEY, listed without an address",
			"node", n.ID, "links", sealed)
	}
	return list, nil
}

func (l *Links) scan(row pgx.Row) (Link, error) {
	var link Link
	var sealed []byte
	if err := row.Scan(&link.ID, &link.NodeID, &sealed, &link.Mode, &link.HasPassword, &link.ExpiresAt, &link.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Link{}, ErrNoLink
		}
		return Link{}, err
	}
	token, err := l.box.Open(sealed, sealContext)
	if errors.Is(err, auth.ErrNoSecretKey) {
		return Link{}, ErrLinksDisabled
	}
	if err != nil {
		// Sealed under another SECRET_KEY (or corrupted): the link stays
		// manageable, only its address is lost.
		return link, nil
	}
	link.Token = token
	return link, nil
}

// linkFor returns linkID if the actor may manage it.
func (l *Links) linkFor(ctx context.Context, actorID, linkID string) (Link, fs.Node, error) {
	if !validUUID(linkID) {
		return Link{}, fs.Node{}, ErrNoLink
	}
	link, err := l.scan(l.svc.db.QueryRow(ctx, `
		SELECT id::text, node_id::text, token_enc, mode, password_hash IS NOT NULL, expires_at, created_at
		FROM links WHERE id = $1`, linkID))
	if err != nil {
		return Link{}, fs.Node{}, err
	}
	n, err := l.svc.sharable(ctx, actorID, link.NodeID)
	if errors.Is(err, fs.ErrNotFound) {
		return Link{}, fs.Node{}, ErrNoLink
	}
	return link, n, err
}

// Update changes a link's mode, password or expiry.
func (l *Links) Update(ctx context.Context, actorID, linkID string, c LinkChange) (Link, error) {
	link, n, err := l.linkFor(ctx, actorID, linkID)
	if err != nil {
		return Link{}, err
	}
	if c.Mode != nil {
		if !validMode(*c.Mode, n) {
			return Link{}, ErrInvalidMode
		}
		link.Mode = *c.Mode
	}
	setPassword := c.Password != nil
	var pw *string
	if setPassword {
		if pw, err = l.hashPassword(ctx, *c.Password); err != nil {
			return Link{}, err
		}
		link.HasPassword = pw != nil
	}
	if c.ExpiresAt != nil {
		link.ExpiresAt = *c.ExpiresAt
	}
	tx, err := l.svc.db.Begin(ctx)
	if err != nil {
		return Link{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		UPDATE links SET mode = $2, expires_at = $3,
			password_hash = CASE WHEN $4 THEN $5 ELSE password_hash END
		WHERE id = $1`, link.ID, link.Mode, link.ExpiresAt, setPassword, pw)
	if err != nil {
		return Link{}, fmt.Errorf("update link: %w", err)
	}
	if setPassword {
		// A new password locks out whoever knew the old one.
		if _, err := tx.Exec(ctx, `DELETE FROM link_sessions WHERE link_id = $1`, link.ID); err != nil {
			return Link{}, err
		}
	}
	return link, tx.Commit(ctx)
}

// Delete revokes a link: it stops working at once.
func (l *Links) Delete(ctx context.Context, actorID, linkID string) error {
	link, _, err := l.linkFor(ctx, actorID, linkID)
	if err != nil {
		return err
	}
	_, err = l.svc.db.Exec(ctx, `DELETE FROM links WHERE id = $1`, link.ID)
	return err
}

// Public is a link as the public side sees it, by its token.
type Public struct {
	ID     string
	NodeID string
	// Name and Kind of the node, shown even to an upload link that may
	// read nothing else.
	Name        string
	Kind        fs.Kind
	Mode        string
	HasPassword bool
	ExpiresAt   *time.Time
	password    *string
}

// Principal is who acts through the link, for fs.
func (p Public) Principal() string { return access.LinkPrincipal(p.ID) }

// Resolve finds the live link a token belongs to; an unknown, revoked or
// expired one is ErrNoLink alike, as is one whose node is gone.
func (l *Links) Resolve(ctx context.Context, token string) (Public, error) {
	var p Public
	err := l.svc.db.QueryRow(ctx, `
		SELECT l.id::text, l.node_id::text, n.name, n.kind, l.mode, l.password_hash, l.expires_at
		FROM links l JOIN nodes n ON n.id = l.node_id
		WHERE l.token_hash = $1 AND (l.expires_at IS NULL OR l.expires_at > now()) AND n.trashed_at IS NULL`,
		tokenHash(token)).Scan(&p.ID, &p.NodeID, &p.Name, &p.Kind, &p.Mode, &p.password, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Public{}, ErrNoLink
	}
	if err != nil {
		return Public{}, err
	}
	p.HasPassword = p.password != nil
	return p, nil
}

// Unlock checks the link's password and opens a session for the browser:
// it returns the cookie value and when it expires.
func (l *Links) Unlock(ctx context.Context, p Public, password string) (string, time.Time, error) {
	if p.password == nil {
		return "", time.Time{}, nil
	}
	ok, err := auth.VerifyPassword(ctx, *p.password, password)
	if err != nil {
		return "", time.Time{}, err
	}
	if !ok {
		return "", time.Time{}, ErrBadPassword
	}
	// Expired sessions of this link go now: cheap, and keeps the table
	// from growing with links that are used a lot.
	if _, err := l.svc.db.Exec(ctx, `DELETE FROM link_sessions WHERE link_id = $1 AND expires_at <= now()`, p.ID); err != nil {
		return "", time.Time{}, err
	}
	var raw [32]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(raw[:])
	session := base64.RawURLEncoding.EncodeToString(raw[:])
	expires := time.Now().Add(LinkSessionTTL)
	if _, err := l.svc.db.Exec(ctx, `INSERT INTO link_sessions (id_hash, link_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash(session), p.ID, expires); err != nil {
		return "", time.Time{}, err
	}
	return session, expires, nil
}

// Unlocked reports whether the browser may use p: no password, or a live
// session from Unlock.
func (l *Links) Unlocked(ctx context.Context, p Public, session string) (bool, error) {
	if !p.HasPassword {
		return true, nil
	}
	if session == "" {
		return false, nil
	}
	var ok bool
	err := l.svc.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM link_sessions WHERE id_hash = $1 AND link_id = $2 AND expires_at > now())`,
		tokenHash(session), p.ID).Scan(&ok)
	return ok, err
}
