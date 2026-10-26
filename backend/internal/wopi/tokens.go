package wopi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TokenTTL is how long an editor may keep a document open on one token.
// Long enough for a working day; access itself is checked on every
// request, so a revoked share or a deleted user stops editing at once.
const TokenTTL = 10 * time.Hour

var errNoToken = errors.New("wopi: unknown or expired access token")

// Tokens issues and resolves WOPI access tokens.
type Tokens struct {
	db *pgxpool.Pool
}

func NewTokens(db *pgxpool.Pool) *Tokens { return &Tokens{db: db} }

// grant is what a token stands for.
type grant struct {
	UserID      string
	DisplayName string
	NodeID      string
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Issue makes a token for userID on nodeID. The caller has checked the
// user may read the node.
func (t *Tokens) Issue(ctx context.Context, userID, nodeID string) (string, time.Time, error) {
	var b [32]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	token := base64.RawURLEncoding.EncodeToString(b[:])
	expires := time.Now().Add(TokenTTL).Truncate(time.Millisecond)
	// Expired tokens go as new ones come: the index makes it cheap.
	if _, err := t.db.Exec(ctx, `DELETE FROM wopi_tokens WHERE expires_at < now()`); err != nil {
		return "", time.Time{}, fmt.Errorf("wopi: prune tokens: %w", err)
	}
	if _, err := t.db.Exec(ctx, `INSERT INTO wopi_tokens (hash, user_id, node_id, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), userID, nodeID, expires); err != nil {
		return "", time.Time{}, fmt.Errorf("wopi: issue token: %w", err)
	}
	return token, expires, nil
}

// resolve returns what a live token grants, for an active user.
func (t *Tokens) resolve(ctx context.Context, token string) (grant, error) {
	var g grant
	err := t.db.QueryRow(ctx, `
		SELECT t.user_id::text, coalesce(nullif(u.display_name, ''), u.username::text), t.node_id::text
		FROM wopi_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.hash = $1 AND t.expires_at > now() AND u.status = 'active'`, hashToken(token)).Scan(&g.UserID, &g.DisplayName, &g.NodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return grant{}, errNoToken
	}
	return g, err
}
