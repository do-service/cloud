package auth

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/config"
)

// Provider is a sign-in provider from auth_providers, secret decrypted.
type Provider struct {
	ID           string
	Slug         string
	Name         string
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	AutoCreate   bool
	LinkByEmail  bool
}

// Providers stores sign-in providers.
type Providers struct {
	db  *pgxpool.Pool
	box *SecretBox
}

func NewProviders(db *pgxpool.Pool, box *SecretBox) *Providers {
	return &Providers{db: db, box: box}
}

// Sync writes the env-configured providers to auth_providers and disables
// those that were configured there before but no longer are. Disabled, not
// deleted: their users' identities stay, and listing the slug again brings
// them back.
func (p *Providers) Sync(ctx context.Context, specs []config.OIDCProvider) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	slugs := make([]string, 0, len(specs))
	for _, s := range specs {
		slugs = append(slugs, s.Slug)
		var secret []byte
		if s.ClientSecret != "" {
			if secret, err = p.box.Seal(s.ClientSecret, s.Slug); err != nil {
				return fmt.Errorf("provider %s: %w", s.Slug, err)
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO auth_providers (slug, name, issuer, client_id, client_secret_enc, scopes, auto_create, link_by_email, from_env)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
			ON CONFLICT (slug) DO UPDATE SET
				name = excluded.name, issuer = excluded.issuer, client_id = excluded.client_id,
				client_secret_enc = excluded.client_secret_enc, scopes = excluded.scopes,
				auto_create = excluded.auto_create, link_by_email = excluded.link_by_email,
				enabled = true, from_env = true, updated_at = now()`,
			s.Slug, s.Name, s.Issuer, s.ClientID, secret, s.Scopes, s.AutoCreate, s.LinkByEmail)
		if err != nil {
			return fmt.Errorf("provider %s: %w", s.Slug, err)
		}
	}
	rows, err := tx.Query(ctx, `
		UPDATE auth_providers SET enabled = false, updated_at = now()
		WHERE from_env AND enabled AND NOT (slug = ANY($1)) RETURNING slug`, slugs)
	if err != nil {
		return fmt.Errorf("disable dropped providers: %w", err)
	}
	dropped, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("disable dropped providers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	for _, slug := range dropped {
		slog.InfoContext(ctx, "disabled OIDC provider no longer in OIDC_PROVIDERS", "provider", slug)
	}
	return nil
}

// Enabled returns the providers people can sign in with, in display order.
func (p *Providers) Enabled(ctx context.Context) ([]Provider, error) {
	rows, err := p.db.Query(ctx, `
		SELECT id, slug, name, issuer, client_id, client_secret_enc, scopes, auto_create, link_by_email
		FROM auth_providers WHERE enabled ORDER BY name, slug`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var pr Provider
		var secret []byte
		if err := rows.Scan(&pr.ID, &pr.Slug, &pr.Name, &pr.Issuer, &pr.ClientID, &secret, &pr.Scopes, &pr.AutoCreate, &pr.LinkByEmail); err != nil {
			return nil, fmt.Errorf("list providers: %w", err)
		}
		if secret != nil {
			if pr.ClientSecret, err = p.box.Open(secret, pr.Slug); err != nil {
				return nil, fmt.Errorf("client secret of provider %s: %w", pr.Slug, err)
			}
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	return out, nil
}
