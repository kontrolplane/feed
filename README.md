<p align="center">
  <h1 align="center">
    <a href="https://kontrolplane.dev">
      <img width="1500" alt="kontrolplane header" src="./assets/kontrolplane-header.svg">
    </a>
  </h1>
</p>

`kontrolplane/feed` is a self-hosted RSS reader built with Go, HTMX, and templ. A single binary serves a server-rendered three-pane UI over plain HTTP. A background worker fetches your feeds on a configurable interval, extracts full article content via readability, and stores everything in SQLite or PostgreSQL. The browser talks directly to Go route handlers that return HTML fragments - there is no client-side state. htmx swaps panes without full page reloads. Your reading data stays in a local database file or your own postgresql instance, nowhere else.

Supports substring search over item titles, authors and abstracts, keyboard-first navigation, timeline filtering (today, yesterday, last week, last month), feed management with inline editing, OPML import/export, dark mode, and deploys to Kubernetes via the included Helm chart with optional CloudNativePG integration.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://github.com/user-attachments/assets/cd1c129a-bf15-402e-864b-1712022a9f31">
  <source media="(prefers-color-scheme: light)" srcset="https://github.com/user-attachments/assets/ae1012c3-2d11-4cec-b32a-986951935da4">
  <img alt="Description of your image" src="https://github.com/user-attachments/assets/ae1012c3-2d11-4cec-b32a-986951935da4">
</picture>

## keyboard shortcuts

| Key | Action | Key | Action |
|---|---|---|---|
| `j` / `k` | next / previous item | `s` | toggle star |
| `o` / `enter` | open item in reader | `m` | toggle read / unread |
| `/` | focus search | `n` | add new feed |
| `1` | toggle sidebar | `2` | toggle item list |

## import / export

kontrolplane/feed supports `opml` for migrating feeds between readers.

- `import`: go to settings and click "import opml" to upload a `.opml` or `.xml` file. Feeds are grouped into folders as defined in the file, existing feeds are updated, new ones are added.
- `export`: click "export opml" in settings to download a `feeds.opml` file containing all your subscriptions grouped by folder, compatible with any reader that supports OPML 2.0.

## configuration

All configuration is via environment variables.

| Variable | Description | Default |
|---|---|---|
| `PORT` | HTTP listen port | `8080` |
| `REFRESH_INTERVAL` | How often the background worker re-fetches every feed | `15m` |
| `RETENTION` | How long a read item is kept before the worker deletes it: `7d`, `30d`, `90d`, or `forever` to never delete. Pruning runs at the end of each refresh cycle. Starred items are exempt at any age. | `30d` |
| `FEEDS_FILE` | Path to an OPML file imported at startup. Existing feeds are updated, new ones added, nothing is removed. Individual unusable entries are skipped and logged; startup fails only if the file itself cannot be read or parsed. | unset |
| `MARK_READ_ON` | Shown in the settings page as the configured reading behaviour. Items are currently marked read when opened regardless of this value. | `open` |
| `DENSITY` | Starting list row height (`tight`, `default`, `loose`) for a browser that has not chosen one. Density is a per-browser preference set on the settings page and stored in `localStorage`; once set there it overrides this. | `default` |
| `DATABASE_DRIVER` | `sqlite` or `postgres` | `sqlite` |
| `DATABASE_PATH` | SQLite file path | `feed.db` |
| `DATABASE_HOST` | PostgreSQL host | `postgres` |
| `DATABASE_PORT` | PostgreSQL port | `5432` |
| `DATABASE_NAME` | PostgreSQL database | `kontrolplane` |
| `DATABASE_USER` | PostgreSQL user | `postgres` |
| `DATABASE_PASSWORD` | PostgreSQL password | `password` |
| `DATABASE_SSL_MODE` | PostgreSQL SSL mode | `disable` |
| `DEVELOPMENT_DEBUG` | Enable debug logging | `false` |
| `DEVELOPMENT_SEED` | Seed a handful of default feeds on startup. Set by `make dev` only; no compose file or image sets it. | `false` |

Search is a case-insensitive substring match (`LIKE '%term%'`) against item title, authors and abstract. It is not a full-text index, so it scans the items table - fine for a personal library, noticeably slower once you are into six figures of items.

## hosting

### docker compose

Two compose files are included for quick self-hosting.

Both pull the published multi-arch image from `ghcr.io`, so no Go toolchain is needed to self-host.

`option: sqlite`:

```bash
docker compose up -d
```

This starts the app on port `8080` with a named volume for the database. All configuration can be adjusted by editing the `environment` block in `docker-compose.yaml`.

`option: postgresql`:

The postgres compose file ships no default password and refuses to start without one. Create a `.env` next to it first (`.env` is gitignored):

```bash
echo "DATABASE_PASSWORD=$(openssl rand -base64 24)" > .env
docker compose -f docker-compose.postgres.yaml up -d
```

This starts the app alongside a PostgreSQL 16 instance. The app waits for Postgres to pass its healthcheck before starting. Postgres is published on `127.0.0.1:5432` only, so it is reachable from the host but not from the network. To start fresh with a clean database:

```bash
docker compose -f docker-compose.postgres.yaml down -v
docker compose -f docker-compose.postgres.yaml up -d
```

To build the image from this checkout instead of pulling the published one, add the `docker-compose.build.yaml` overlay:

```bash
docker compose -f docker-compose.yaml -f docker-compose.build.yaml up -d --build
```

### helm chart

A Helm chart is available at [`kontrolplane/helm-charts`](https://github.com/kontrolplane/helm-charts).

`option: sqlite`:

```bash
helm repo add kontrolplane https://kontrolplane.github.io/helm-charts
helm install feed kontrolplane/feed
```

`option: postgresql through cnpg`

```bash
helm install feed kontrolplane/feed \
  --set database.driver=postgres \
  --set cnpg.enabled=true
```

This creates a Cloud Native PostgreSQL (CNPG) `Cluster` resource alongside the app. The CNPG operator must be installed on the cluster beforehand. Credentials are wired automatically from the operator-generated secret.

`option: postgresql external`

```bash
helm install feed kontrolplane/feed \
  --set database.driver=postgres \
  --set database.postgres.host=pg.example.com \
  --set database.postgres.user=feed \
  --set database.postgres.password=secret \
  --set database.postgres.database=feed
```

Or reference an existing Kubernetes secret:

```bash
helm install feed kontrolplane/feed \
  --set database.driver=postgres \
  --set database.postgres.existingSecret=pg-credentials
```

See the full chart documentation at [`kontrolplane/helm-charts/feed`](https://github.com/kontrolplane/helm-charts/tree/main/feed) for all configuration options including ingress, resources, and autoscaling.

## prerequisites

- go 1.25+ (the `go` directive in `go.mod` is the source of truth)
- [templ](https://templ.guide/) - install it with `make deps`, which pins the CLI to the `github.com/a-h/templ` version in `go.mod`. Do not use `@latest`: a CLI newer than the runtime library generates code that library cannot compile.

## development

```bash
make deps
make dev
```

Open [http://localhost:8080](http://localhost:8080).

`make dev` runs the server under air, which sets `DEVELOPMENT_SEED=true`, so a development run starts with a handful of default feeds. Nothing else sets that flag - a fresh compose or Helm install starts empty. Add feeds through the UI, import an OPML file from settings, or point `FEEDS_FILE` at one.

Before opening a pull request:

```bash
make check                 # gofmt, go vet, go test
go test ./... -race        # what CI actually runs
```

CI runs `go vet ./...`, `go build ./...`, `go test ./... -race` and a `gofmt` check on every pull request. `-race` is not cosmetic: shared handler state has produced a real data race here before.

A second CI job regenerates the templ output with the pinned CLI and fails if it differs from what is committed. The `*_templ.go` files are committed *and* regenerated during the image build, so if you edit a `.templ` file, run `make generate` and commit the regenerated Go in the same change.

## license

MIT

<p align="center">
  <a href="https://kontrolplane.dev">
    <img width="1500" alt="kontrolplane footer" src="./assets/kontrolplane-footer.svg">
  </a>
</p>
