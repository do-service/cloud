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

### Files in the browser

- **File manager**: a virtualized list that stays responsive on 50 000-item
  folders, sorting kept in the URL, multi-select with keyboard shortcuts,
  new folder, rename (F2), move and copy through a folder picker, delete to
  trash with undo, search by name.
- **Live updates**: changes made elsewhere — another tab, WebDAV, the desktop
  client, a colleague in a shared folder — show up without a page reload.
- **Previews**: thumbnails in the list and a full-screen viewer for images
  (HEIC included) and PDF, rendered by `vips` on first request.
- **Uploads** over [tus](https://tus.io/): resumable, three files at a time,
  whole folders with their structure, a progress panel; big uploads survive
  dropped connections and server restarts.
- **Downloads** of files (with HTTP Range) and of whole folders as ZIP.
- **Versions** of every file with download and restore, and a **trash** with
  restore and automatic cleanup (`TRASH_RETENTION`, `VERSIONS_MAX_COUNT`,
  `VERSIONS_MAX_AGE`). Quotas per user.

### Sharing

- With **users and groups**, read-only or with write rights; what others
  shared with you is under «Доступно мне» (Shared with me).
- **Public links** with an optional password, an expiry date and a
  view / download / upload-only mode, opened on a page that needs no account.
- A single access check (`internal/access`) behind the web API, WebDAV, the
  editors and sync, covered by a generated
  [security matrix](docs/security-matrix.md).

### Office documents

- **Online editing** of docx/xlsx/pptx/odt through WOPI: OnlyOffice Docs by
  default, Collabora Online as an option. Saves become new versions; WOPI
  locks and proof keys are checked.
- **New document** from the UI: an empty document, spreadsheet or
  presentation, opened in the editor.

### Sync

Three ways to keep files on a computer in step with the cloud:

- **Desktop client** (`desktop/`, Go + Wails v3):
  a window and a tray icon.
  - **Sign-in in the browser**, on the cloud's own page (password, OIDC — the
    same as on the web), with PKCE over loopback; the app never sees the
    password and gets a device token that can be revoked from the settings.
  - **Selective sync**: browse the cloud with thumbnails and tick the files
    and folders you want on this computer — nothing is downloaded until you
    choose. A ticked folder brings everything below it, including what is
    added later.
  - **Two-way, near real time**: the server keeps a change journal per user,
    the client long-polls it (a change on the server reaches the client in
    under 2 s) and watches the local folder (FSEvents on macOS).
  - **Safe by design**: a three-way comparison (last synced state, local,
    remote) with conditional writes (`If-Match`), so nothing is silently
    overwritten. When both sides change a file, the server version keeps its
    name and the local one is saved and uploaded as
    `Name (конфликт, Device, date).ext`. Deletes go to the cloud trash.
    Mass deletes (over 500 files or 30 % of what is synced) pause and ask.
  - **Robust transfers**: resumable uploads and downloads that survive a
    restart, downloads verified by sha256 and written to a temporary file
    first, an operation journal that survives `kill -9`, offline mode with
    back-off, pause, bandwidth limit.
  - **Shared folders** under «Доступно мне», read-only ones stay read-only.
  - **Several clouds and accounts** at once, each with its own folder.
  - Builds: macOS (universal DMG) today; Windows and Linux builds are in
    progress. See [desktop/README.md](desktop/README.md).
- **Folder sync in the browser**: in Chrome or Edge on a computer, pick a
  local folder and the cloud folders to keep there; a button runs a two-way
  sync with conflict copies and a confirmation for deletes (File System
  Access API, no install needed; runs while the tab is open).
- **WebDAV** at `/remote.php/dav/files/<user>/` (and `/dav/files/<user>/`)
  with app passwords and locks, checked against litmus and rclone in CI;
  meant for Finder, Windows Explorer, GNOME Files, davfs2 and mobile WebDAV
  apps (manual checklist: [docs/webdav-clients.md](docs/webdav-clients.md)).

### Accounts and administration

- **Sign-in** with local accounts and any number of OIDC providers (PKCE,
  just-in-time accounts), sessions with CSRF protection, app passwords for
  WebDAV, signed-in devices with revoke.
- **Admin UI**: users (create, block, quota, delete), groups, an audit log
  of who did what, system status.
- **Security headers**: a strict Content Security Policy, real client IPs
  behind trusted proxies (`TRUSTED_PROXIES`), rate-limited sign-in.

### Operations

- **One container** next to PostgreSQL: the SPA is embedded into the Go
  binary; images for `linux/amd64` and `linux/arm64` on
  `ghcr.io/do-service/cloud`.
- **Files stay readable**: on the local file system the current versions
  are kept as a plain directory tree that mirrors the folders, so data is
  accessible without the app.
- **Background jobs** (cleanup of uploads, trash, versions, the sync
  journal) in the same binary or in separate workers (`ROLE=api|worker`).
- **Prometheus metrics** on an internal port, health checks
  (`/readyz`).
- **Benchmarked** at 20 million nodes and 2 000 users (`bench/`); a smaller
  listing benchmark runs in CI.

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
