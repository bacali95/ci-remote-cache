# nx-remote-cache

Self-hosted remote cache server for [Nx](https://nx.dev), implementing the
HTTP contract from
[nx.dev/docs/kb/self-hosted-caching](https://nx.dev/docs/kb/self-hosted-caching#build-your-own-caching-server)
in Go, with an embedded admin UI (React + shadcn/ui) for browsing/pruning
cache entries and managing users and access tokens.

## Cache API (data plane)

| Method | Path              | Auth              | Success | Failure                              |
|--------|-------------------|-------------------|---------|---------------------------------------|
| PUT    | `/v1/cache/{hash}` | Bearer, write scope | 200    | 401 no/bad token · 403 read-only token · 409 hash already stored · 411 missing Content-Length |
| GET    | `/v1/cache/{hash}` | Bearer, read scope  | 200 + binary body | 401 no/bad token · 403 forbidden · 404 not found |
| GET    | `/health`         | none              | 200     | —                                      |

Cache entries are content-addressed and immutable: a hash is written once
(`PUT`) and never overwritten — a second `PUT` for the same hash returns 409.

The same server also implements the
[Turborepo remote cache API](https://turborepo.dev/api/remote-cache-spec)
under `/v8/artifacts` (the `turbo` client adds the `/v8` prefix itself):

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| GET    | `/v8/artifacts/status` | Bearer, read | `{"status":"enabled"}` |
| HEAD / GET | `/v8/artifacts/{hash}` | Bearer, read | returns stored `x-artifact-*` headers (incl. the signature tag) |
| PUT    | `/v8/artifacts/{hash}` | Bearer, write | 202; re-uploading an existing hash is also 202 |
| POST   | `/v8/artifacts` | Bearer, read | batch lookup of size / duration / tag |
| POST   | `/v8/artifacts/events` | Bearer, read | hit/miss analytics, validated and acknowledged |

Both protocols are always on, use the same tokens, and store artifacts in
separate namespaces (`nx/<hash>`, `turbo/<hash>`), so they never collide.

## Admin UI

Visit `/admin` on your running server. There's no public sign-up — the
first login comes from the bootstrap admin (see `ADMIN_BOOTSTRAP_EMAIL` /
`ADMIN_BOOTSTRAP_PASSWORD` below); from there, create more admin accounts
and access tokens from the UI itself.

- **Cache** — paginated list of stored entries (hash, size, last modified),
  delete one or select many for bulk delete, or prune everything older than
  N days.
- **Tokens** — create/revoke bearer tokens used by CI (`read` or `write`
  scope). The raw token is shown once at creation time and never again.
- **Users** — create/delete admin accounts, change your own password.
- **Settings** — storage backend (local/S3/GCS) and its credentials,
  session lifetime, max cache entry size. See below.

All cache-access tokens, admin users, and runtime settings live in
Postgres — there's no env-var token list or storage config anymore (see
[Authentication](#authentication) and
[Runtime settings](#runtime-settings-storage-backend-session-ttl-max-entry-size)
below).

## Runtime settings: storage backend, session TTL, max entry size

These are **not** env vars — they're configured from the admin UI's
Settings page, stored in Postgres, and take effect immediately on save,
with no restart. Changing the storage backend is connectivity-tested
before it's applied; if the test fails (bad bucket, bad credentials,
unreachable endpoint), nothing changes.

- **Local disk** — a directory path. Suited to a single always-on instance
  with a persistent volume. Defaults to `./data` (which resolves to `/data`
  in the Docker image, matching its mounted volume).
- **S3** — bucket, region, prefix, endpoint (for R2/MinIO), path-style
  flag, and optionally a static access key/secret pair. Leave the
  credentials blank to use the AWS default credential chain (IAM role, env
  vars on the host) instead.
- **GCS** — bucket, prefix, and optionally a service-account key JSON.
  Leave it blank to use Application Default Credentials (workload
  identity, `gcloud auth application-default login`) instead.

Use S3/GCS when the server runs as multiple replicas or on ephemeral
infrastructure (recommended for anything backing CI at scale) — a bucket
is the shared durable store that survives any single instance restarting.

Static cloud credentials entered here are encrypted (AES-256-GCM) before
being stored in Postgres — see `SETTINGS_ENCRYPTION_KEY` below. The raw
secret is never sent back to the browser after saving; the Settings page
shows "configured" and lets you clear or replace it, not view it.

The admin UI's "prune by age" and "browse" features scan every entry (most
object stores have no server-side "modified before X" filter) — fine for a
self-hosted cache, but expect a large S3/GCS bucket to take a while to
prune in one call.

## Configuration (env vars)

What's left in env vars is genuinely infra-level — where the database is,
how the process listens — plus one encryption key that must not itself
live in the database it protects.

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `3000` | listen port |
| `DATABASE_URL` | — | **required.** Postgres connection string, e.g. `postgres://user:pass@host:5432/nxcache?sslmode=disable` |
| `SETTINGS_ENCRYPTION_KEY` | — | **required.** Base64, 32 bytes. Encrypts cloud credentials at rest. Generate with `openssl rand -base64 32`. Losing/rotating this key makes any previously-saved cloud credentials undecryptable — you'd need to re-enter them from the Settings page. |
| `ADMIN_BOOTSTRAP_EMAIL` / `ADMIN_BOOTSTRAP_PASSWORD` | — | creates one admin user on startup, only if the `users` table is empty. Safe to leave set indefinitely — it's a no-op once any user exists. |
| `COOKIE_SECURE` | `true` | set `false` only for local `http://` development; must be `true` once served over TLS |

Migrations run automatically on startup (embedded, tracked in a
`schema_migrations` table — safe to run on every boot).

## Running locally

```bash
docker compose up --build
```

This brings up Postgres alongside the server and creates
`admin@example.com` / `change-me-immediately` (override via
`ADMIN_BOOTSTRAP_EMAIL`/`ADMIN_BOOTSTRAP_PASSWORD` env vars, or a `.env`
file) — log in at `http://localhost:3000/admin` and change it immediately
from the Users page.

Without Docker, point `make run` at a Postgres instance you already have
running:

```bash
DATABASE_URL=postgres://... SETTINGS_ENCRYPTION_KEY=$(openssl rand -base64 32) \
  ADMIN_BOOTSTRAP_EMAIL=admin@example.com ADMIN_BOOTSTRAP_PASSWORD=changeme \
  COOKIE_SECURE=false make run
```

The frontend needs a one-time build before `go build`/`go run` will work at
all — `go:embed` needs `web/dist` to exist (this is what `make build`/`make
run` do for you; see the Makefile):

```bash
cd web && bun install --frozen-lockfile && bun run build
```

## Authentication

**Cache tokens** (used by CI, in the `Authorization: Bearer` header) are
created from the admin UI's Tokens page. Two scopes exist because CI has
two trust levels:

- **Write tokens** can upload and download. Only give these to jobs you
  trust to publish good artifacts — typically pushes to `main`/protected
  branches, via a token stored in a repository secret.
- **Read tokens** can only download. Give these to untrusted contexts —
  e.g. builds triggered by pull requests from forks — so a malicious fork
  PR can benefit from the cache but can never poison it.

Tokens are stored as a SHA-256 hash; the raw value is shown once at
creation and can't be recovered afterward — revoke and recreate if lost.
Revoking is immediate (next request with that token gets 401).

**Admin logins** use bcrypt-hashed passwords and server-side sessions (a
random session ID in an `httpOnly`, `SameSite=Strict` cookie; only its hash
is stored, same pattern as cache tokens). Mutating admin API requests also
require a custom header the browser JS sets, which a cross-site form
submission can't forge — a second, independent layer on top of
`SameSite=Strict`. Login attempts are rate-limited per IP.

This server has no built-in TLS. Terminate TLS at a reverse proxy or load
balancer in front of it and only expose the plaintext port on a private
network — bearer tokens and admin sessions must never travel over plain
HTTP on the public internet. Set `COOKIE_SECURE=true` (the default) once
TLS is in place.

## Nx client configuration

No `nx.json` changes are required. Point the Nx client at the server via
environment variables in your CI job (and locally, if desired):

```bash
export NX_SELF_HOSTED_REMOTE_CACHE_SERVER=https://nx-cache.example.com
export NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN=$CACHE_TOKEN
```

## Turborepo client configuration

```bash
export TURBO_API=https://nx-cache.example.com
export TURBO_TOKEN=$CACHE_TOKEN
export TURBO_TEAM=any-name   # required by turbo to enable remote caching; its value is ignored
```

Optionally sign artifacts by setting `"remoteCache": { "signature": true }`
in `turbo.json` and `TURBO_REMOTE_CACHE_SIGNATURE_KEY` (32+ bytes) in CI.

## CI/CD for this repo

- `.github/workflows/ci.yml` — on every push/PR: builds the admin UI, then
  `go vet`, `go build`, `go test -race -cover` (against a Postgres service
  container), `golangci-lint`, and a Docker build-only check.
- `.github/workflows/docker-publish.yml` — on push of a `vX.Y.Z` tag only:
  builds a multi-arch (amd64/arm64) image (Node build stage → Go build
  stage → distroless runtime) and pushes it to `ghcr.io/<owner>/<repo>`.
  Merges to `main` run CI but do not publish an image — cut a tag (`git tag
  v0.1.0 && git push origin v0.1.0`) when you want a new release.

Deploy by pulling the published image and running it with your storage/DB
env vars set (e.g. as a systemd unit, a Kubernetes Deployment, or an ECS
service), pointed at a real Postgres instance, behind a TLS-terminating
load balancer.

Running the Go test suite locally needs a real Postgres (several packages
run integration tests against it, skipped automatically if unset):

```bash
docker run --rm -e POSTGRES_PASSWORD=test -p 5432:5432 postgres:16-alpine &
TEST_DATABASE_URL="postgres://postgres:test@localhost:5432/postgres?sslmode=disable" \
  go test ./... -race -cover -p 1
```

`-p 1` matters: several packages share that one Postgres instance and
truncate its tables at the start of each test, which races if package
binaries run in parallel.

## Example: consumer repo workflow

A separate repo building with Nx would use the cache like this:

```yaml
# .github/workflows/ci.yml (in the Nx workspace repo)
jobs:
  build:
    runs-on: ubuntu-latest
    env:
      NX_SELF_HOSTED_REMOTE_CACHE_SERVER: https://nx-cache.example.com
      # Write token only available on trusted refs; fork PRs fall back to
      # the read-only token (or no token) and simply get cache misses on
      # write, cache hits on read.
      NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN: >-
        ${{ github.event_name == 'pull_request' && github.event.pull_request.head.repo.fork
            && secrets.NX_CACHE_READ_TOKEN
            || secrets.NX_CACHE_WRITE_TOKEN }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
      - run: npm ci
      - run: npx nx affected -t build test lint
```

Create `NX_CACHE_READ_TOKEN` and `NX_CACHE_WRITE_TOKEN` from this server's
admin UI (Tokens page), then store the raw values as repository (or
organization) secrets in the consumer repo.
