-- +goose Up
CREATE TABLE auth_providers (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug              text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,31}$'),
    name              text NOT NULL,
    type              text NOT NULL DEFAULT 'oidc' CHECK (type IN ('oidc')),
    issuer            text NOT NULL,
    client_id         text NOT NULL,
    -- AES-256-GCM under SECRET_KEY, bound to the slug; NULL for a public
    -- client. Encrypted so a database dump alone doesn't let anyone pose
    -- as this app to the provider.
    client_secret_enc bytea,
    scopes            text[] NOT NULL DEFAULT '{openid,profile,email}',
    enabled           boolean NOT NULL DEFAULT true,
    auto_create       boolean NOT NULL DEFAULT true,
    link_by_email     boolean NOT NULL DEFAULT false,
    -- Rows configured through OIDC_* are rewritten from the env on every
    -- start and disabled once dropped from it; the rest belong to the
    -- admin UI.
    from_env          boolean NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identities (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    -- No cascade: deleting a provider that people sign in with must be a
    -- deliberate step (disable it, or remove its identities first).
    provider_id   uuid NOT NULL REFERENCES auth_providers,
    -- The provider's stable user id; emails and usernames can change.
    subject       text NOT NULL,
    email         citext,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, subject)
);

-- Cascade from users and "sign-in methods" in the profile.
CREATE INDEX identities_user ON identities (user_id);

-- +goose Down
DROP TABLE identities;
DROP TABLE auth_providers;
