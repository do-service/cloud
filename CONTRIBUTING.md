# Contributing

Thanks for your interest in Cloud! Bug reports, fixes and features are all
welcome. For anything bigger than a small fix, please open an issue first so
we can agree on the approach before you spend time on it.

The design docs ([`docs/`](docs/)) and the UI are in Russian; issues and pull
requests in English or Russian are both fine.

## Development setup

You need Docker with Compose v2.24+, Go (the version in `backend/go.mod`) and
Node.js 24.

```sh
docker compose up -d postgres          # PostgreSQL on 127.0.0.1:55433 (databases cloud and cloud_test)
cd backend && make run                 # API on :8090, metrics on :9091
cd frontend && npm ci && npm run dev   # http://localhost:5173, proxies the API to :8090
```

The whole stack, the way an admin runs it: `docker compose up -d --build`,
then <http://localhost:8090>. Changes should work there too, not only in
tests.

The desktop client has its own instructions in
[desktop/README.md](desktop/README.md).

## Checks

Run what CI runs for the parts you touched:

```sh
cd backend && make lint test         # gofmt + go vet, tests (-p 1, against cloud_test)
cd backend && make test-webdav       # WebDAV compatibility (litmus + rclone), needs the compose stack
cd backend && make test-e2e          # OIDC sign-in, needs `docker compose --profile e2e up -d keycloak`
cd frontend && npx tsc -b && npm run test && npm run build
cd desktop && make lint test
```

If you change access rules, regenerate the security matrix and review its
diff: `cd backend && go test ./cmd/server -run SecurityMatrix -update`
(updates `docs/security-matrix.md`).

## Code conventions

- Backend: standard library `net/http`, pgx v5, goose migrations in
  `backend/internal/db/migrations/NNNN_name.sql`, SQL written by hand in
  `repo.go` files (no ORM), configuration from the environment only
  (`internal/config`), `slog` logging, API errors as `{"code","message"}`
  through `internal/httpx`.
- Frontend: React 19, strict TypeScript, Mantine, TanStack Query,
  react-router. All user-facing strings go through `t()`
  (`frontend/src/i18n`). Error codes in `frontend/src/api.ts` (`ERROR_CODES`)
  must match `backend/internal/httpx`.
- No `any`, `as` casts or `@ts-ignore` to silence type errors.
- Code comments are in English and explain *why*, not *what*.
- Keep changes focused: no drive-by refactoring or reformatting.

## Commits and pull requests

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):
`feat: ...`, `fix: ...`, `docs: ...`, `ci: ...`, `refactor: ...`,
`test: ...`, `build: ...`, `chore: ...` — short, in the imperative.

1. Fork the repository and create a branch from `main`.
2. Make the change with tests and run the checks above.
3. Open a pull request against `main` and fill in the template. CI must be
   green before review.
4. Address review comments with new commits; maintainers squash or rebase on
   merge.

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
