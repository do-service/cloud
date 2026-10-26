# Cloud

[![CI](https://github.com/do-service/cloud/actions/workflows/ci.yml/badge.svg)](https://github.com/do-service/cloud/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

A self-hosted file cloud with the core of Nextcloud — files, sharing, versions,
trash, sync and online editing of office documents — in a single Go binary
next to PostgreSQL. It installs with one `docker compose up` and stays fast on
small servers and on folders with tens of thousands of files.

> **Status: alpha.** The server and the web UI work end to end, but the API,
> the database schema and the configuration may still change between releases
> without a migration path. The UI is in **Russian only** for now (every
> string already goes through `t()` in `frontend/src/i18n`, so translations
> are welcome). The design docs in [`docs/`](docs/) are in Russian too.

## Features

- **Files and folders** in the browser: a virtualized list that stays
  responsive on 50 000-item folders, sorting, multi-select, create, rename,
  move, copy, delete to trash with undo, search by name, download of files and
  whole folders as ZIP, previews and thumbnails (via `vips`: images incl. HEIC,
  PDF).
- **Resumable uploads** over [tus](https://tus.io/), including whole folders
  with their structure; big uploads survive dropped connections and server
  restarts.
- **Versions** of every file with restore, and a **trash** with restore and
  retention (`TRASH_RETENTION`, `VERSIONS_MAX_COUNT`, `VERSIONS_MAX_AGE`).
- **Sharing** with users and groups (read-only or with write rights), and
  **public links** with an optional password, an expiry date and a
  view / download / upload-only mode.
- **WebDAV** at `/remote.php/dav/files/<user>/` (and `/dav/files/<user>/`)
  with app passwords, checked against litmus and rclone in CI; works with
  existing WebDAV clients ([docs/webdav-clients.md](docs/webdav-clients.md)).
- **Online editing** of docx/xlsx/pptx/odt through WOPI: OnlyOffice Docs by
  default, Collabora Online as an option. Saves become new versions.
- **Sign-in** with local accounts and any number of OIDC providers (PKCE,
  just-in-time accounts), sessions with CSRF protection.
- **Administration** in the web UI: users, quotas, groups, an audit log and
  system status.
- **Files stay readable**: on the local file system the current versions are
  kept as a plain directory tree that mirrors the folders, so data is
  accessible without the app.
- **Desktop sync client** (`desktop/`, Go + Wails v3, macOS first): select
  files and folders to keep in sync both ways, several clouds and accounts
  at once. Early stage, see [desktop/README.md](desktop/README.md).

## Quick start

Requirements: Docker Engine with Docker Compose v2.24 or newer.

```sh
git clone https://github.com/do-service/cloud.git
cd cloud
ADMIN_USERNAME=admin ADMIN_PASSWORD='change-me-please' docker compose up -d --build
```

Open <http://localhost:8090> (the host port is `CLOUD_PORT`) and sign in as
the admin. The first administrator is created from `ADMIN_USERNAME` and
`ADMIN_PASSWORD` (at least 8 characters) on an empty database only; once an
admin exists, these variables are ignored. Database migrations run when the
container starts; files live in the `cloud_data` volume.

The default stack is `postgres`, `cloud` and `onlyoffice` (the editor at
<http://localhost:9981>; its image is about 3 GB and needs ~1 GB of RAM).
Everything is bound to `127.0.0.1`: for a real server put a TLS reverse proxy
in front, as described in the [administrator guide](docs/admin-guide.md)
(Russian) — installation, HTTPS with Caddy or nginx, editors, OIDC, backup
and upgrade.

### Configuration

Variables used in `docker-compose.yml` (`CLOUD_PORT`, `POSTGRES_PASSWORD`,
`PUBLIC_BASE_URL`, `ADMIN_*`, retention, quotas, `TRUSTED_PROXIES`, `WOPI_*`)
come from the shell or a `.env` file next to it. Secrets and OIDC providers go
into an optional `cloud.env`, passed to the `cloud` container as a whole:

```sh
# cloud.env (keep it out of version control)
SECRET_KEY=...                 # openssl rand -base64 32; encrypts secrets in the database
OIDC_PROVIDERS=kc              # comma-separated slugs
OIDC_KC_ISSUER=https://id.example.com/realms/main
OIDC_KC_CLIENT_ID=cloud
OIDC_KC_CLIENT_SECRET=...
OIDC_KC_NAME=Keycloak          # also _SCOPES, _AUTO_CREATE, _LINK_BY_EMAIL
```

Sign-in through a provider starts at `GET /api/v1/auth/oidc/<slug>/start`.
Main server variables:

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `METRICS_ADDR` | `:9090` | Prometheus `/metrics` (unauthenticated, not published by compose); empty turns it off |
| `DATABASE_URL` | `postgres://cloud:cloud@localhost:55433/cloud?sslmode=disable` | PostgreSQL (compose builds it from `POSTGRES_*`) |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `20` / `2` | Connection pool size |
| `DB_STATEMENT_TIMEOUT` | `30s` | Per-query limit; `0` turns it off |
| `PUBLIC_BASE_URL` | `http://localhost:5173` (compose: `http://localhost:8090`) | The address the browser uses |
| `DATA_DIR` | `./data` (image: `/data`) | File storage directory |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | Logging (`text` for local debugging) |

The full list is in the
[administrator guide](docs/admin-guide.md#переменные-окружения).

### Optional profiles

- **Collabora Online** instead of OnlyOffice (<http://localhost:9980>):

  ```sh
  WOPI_DISCOVERY_URL=http://collabora:9980/hosting/discovery WOPI_EDITOR_URL= \
    docker compose --profile collabora up -d
  ```

- **Keycloak** for development and the OIDC end-to-end test
  (<http://127.0.0.1:8180>, admin/admin; realm `cloud` with user
  `ivan` / `ivanpass` is imported on start):

  ```sh
  docker compose --profile e2e up -d keycloak
  ```

## Development

```sh
docker compose up -d postgres   # PostgreSQL on 127.0.0.1:55433, plus a cloud_test database for tests
cd backend && make run          # API on :8090, metrics on :9091
cd frontend && npm run dev      # http://localhost:5173, proxies the API to :8090
```

Checks (the same as CI):

```sh
cd backend && make lint test                               # gofmt, go vet, tests (needs the postgres above)
cd backend && make test-webdav                             # litmus + rclone against a throwaway server
cd backend && make test-e2e                                # OIDC sign-in against the Keycloak profile
cd frontend && npx tsc -b && npm run test && npm run build
cd desktop && make lint test                               # desktop client (see desktop/README.md)
```

The benchmark stand (20M nodes, k6 scenarios) is described in
[bench/README.md](bench/README.md). See [CONTRIBUTING.md](CONTRIBUTING.md)
for the workflow.

### Repository layout

```
backend/    Go server: REST API, WebDAV, WOPI host, background jobs; embeds the SPA
frontend/   React 19 + TypeScript + Mantine web UI
desktop/    desktop sync client (Go + Wails v3)
bench/      benchmark generator and k6 scenarios
deploy/     configs for the compose services (Keycloak realm, OnlyOffice, Postgres init)
docs/       requirements, architecture, admin guide (in Russian)
```

Architecture: [docs/architecture.md](docs/architecture.md) (modules, database
schema, storage, WebDAV/WOPI, risks); requirements: [docs/prd.md](docs/prd.md);
security model: [docs/security-matrix.md](docs/security-matrix.md).

## Security

Please report vulnerabilities privately, see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). OnlyOffice Docs (AGPL v3) and Collabora Online
(MPL 2.0) run as separate, unmodified containers and are not part of this
code base.
