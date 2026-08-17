# pooml - Logs + Metrics in One Binary

Pooml (stands for "Poor Man's Logger") is a self-hosted, single-binary log and metrics server on top of SQLite, built for solo developers and small teams who want observability for their side projects and VPS deployments without running an ELK stack, a Prometheus + Grafana pair, or paying per-GB SaaS prices.

One binary, two SQLite files, a web UI. Ship logs from FluentBit or plain HTTP, push metrics via OTLP or let pooml scrape Prometheus endpoints, query everything with real SQL, draw charts, build dashboards, and get alerts on your phone.

The assumption is that you will self-host pooml on a VPS or a small cloud instance (Coolify, plain Docker, a systemd unit - anything works).

## What it does

- **Logs**: FluentBit-compatible HTTP ingestion with automatic format detection per line (JSON, Common Log Format, plain text), full-text search (SQLite FTS5), live tail, infinite scroll, SQL querying
- **Metrics**: OTLP/HTTP push and Prometheus scrape targets, a quick-query builder that compiles to SQL (`increase(http_requests_total) per 1h last 24h by service`), charts. Two metric types on purpose: counters and gauges - histograms and summaries are downcast to their `_sum`/`_count` counter series
- **Dashboards**: typed logs/metrics dashboards with stream, chart, and number panels
- **Alerts**: SQL-based alert rules on either signal, delivered via Pushover or Once Campfire
- **Ops**: hourly retention cleanup, scheduled S3 backups, Prometheus `/metrics` self-observability - pooml can monitor itself
- **AI-ready (MCP)**: [`@pooml/mcp`](https://www.npmjs.com/package/@pooml/mcp) gives Claude (or any MCP client) read-only SQL access to your logs and metrics - LLMs already speak SQL, so there's no query DSL for the model to hallucinate around
- **One login**: a single shared secret for the UI, API keys for ingestion

## Open-source but closed-contribution

Pooml is open-source, but I do not accept external contributions.
This is a personal project, and I want to keep full control over the codebase and direction of the project.
Also, I don't feel I have enough free time to review and manage contributions (if any).

Therefore, please do not open PRs.

If you find any bugs, have questions or suggestions, use the `Discussions` section for that.

## Quick start

### Docker

```bash
docker run -d \
  --name pooml \
  --restart unless-stopped \
  -e POOML_UI_SECRET=your-ui-login-secret-min-32-chars-long \
  -e POOML_ENCRYPTION_KEY=your-encryption-key-min-32-chars-long \
  -e POOML_DB_DIR=/data \
  -p 8080:8080 \
  -p 8081:8081 \
  -v pooml-data:/data \
  mykonordy/pooml:latest
```

Open `http://localhost:8081`, log in with your `POOML_UI_SECRET`, create an API key in Settings, and point your log shipper at port 8080.

Port 8080 is the ingestion API, port 8081 is the UI. The two are separate servers on purpose: expose the API to your services, keep the UI behind your VPN or reverse proxy.

### Docker Compose

```yaml
services:
  pooml:
    image: mykonordy/pooml:latest
    container_name: pooml
    restart: unless-stopped
    environment:
      POOML_UI_SECRET: your-ui-login-secret-min-32-chars-long
      POOML_ENCRYPTION_KEY: your-encryption-key-min-32-chars-long
      POOML_DB_DIR: /data
    ports:
      - "8080:8080"
      - "8081:8081"
    volumes:
      - pooml-data:/data
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:8080/healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3

volumes:
  pooml-data:
```

`docker compose up -d`, and the same two ports apply. Both examples use a named volume on purpose: pooml runs as a non-root user (uid 1001), and a bind-mounted host directory is created root-owned on Linux, which crash-loops the container. If you prefer a bind mount, `chown -R 1001:1001` the host directory first. This is also the shape platforms like Coolify accept as a one-service compose deployment.

### Binary (Linux)

Download the archive for your architecture from [Releases](https://github.com/n0rdy/pooml/releases), then:

```bash
tar -xzf pooml_Linux_x86_64.tar.gz
chmod +x pooml-linux-amd64 && sudo mv pooml-linux-amd64 /usr/local/bin/pooml

POOML_DB_DIR=~/pooml-data \
POOML_UI_SECRET=your-ui-login-secret-min-32-chars-long \
POOML_ENCRYPTION_KEY=your-encryption-key-min-32-chars-long \
pooml
```

### From source

Requires Go 1.26+ and a C compiler (CGO). The `sqlite_fts5` build tag is mandatory - without it the binary builds but fails at startup with `no such module: fts5`.

```bash
git clone https://github.com/n0rdy/pooml && cd pooml
go build -tags sqlite_fts5 .
```

## Configuration

All configuration is via environment variables. Required variables fail fast at startup with a clear message.

| Variable                         | Default          | Purpose                                                                                                                                              |
|----------------------------------|------------------|------------------------------------------------------------------------------------------------------------------------------------------------------|
| `POOML_DB_DIR`                   | *required*       | Directory for `logs.db`, `metrics.db`, and `meta.db`. Created if missing; `~` expands.                                                               |
| `POOML_UI_SECRET`                | *required*       | UI login secret, min 32 chars.                                                                                                                       |
| `POOML_ENCRYPTION_KEY`           | *required*       | Encrypts secrets stored in `meta.db` (S3 credentials, notification tokens), min 32 chars. Losing it means re-entering those secrets.                 |
| `POOML_ENV`                      | `pro`            | `local` / `pro`. Affects cookie Secure flag, HSTS, log verbosity.                                                                                    |
| `POOML_API_ADDR`                 | `localhost:8080` | Ingestion API bind address.                                                                                                                          |
| `POOML_UI_ADDR`                  | `localhost:8081` | UI bind address.                                                                                                                                     |
| `POOML_LOG_LEVEL`                | `info`           | Pooml's own log level (`trace` / `debug` / `info` / `warn` / `error`).                                                                               |
| `POOML_TRUST_PROXY_HEADERS`      | `false`          | Derive client IP from `X-Forwarded-For` for throttling. Enable ONLY behind a reverse proxy that overrides the header, otherwise IPs are spoofable.   |
| `POOML_METRICS_ENABLED`          | `false`          | Expose pooml's own Prometheus metrics at `:8080/metrics` and register a self-scrape target.                                                          |
| `POOML_METRICS_AUTH_SECRET`      | -                | Required when metrics are enabled, min 32 chars. Sent as `X-API-Key` by scrapers.                                                                    |
| `POOML_QUERY_API_ENABLED`        | `false`          | Expose the read-only SQL query API at `/api/v1/query/*`.                                                                                             |
| `POOML_QUERY_AUTH_SECRET`        | -                | Required when the query API is enabled, min 32 chars. Sent as `X-API-Key`. Deliberately separate from ingest keys.                                   |
| `POOML_SHUTDOWN_TIMEOUT_SECONDS` | `30`             | Hard deadline for graceful shutdown.                                                                                                                 |

## Shipping logs

### FluentBit

```ini
[OUTPUT]
    Name http
    Match *
    Host your-pooml-host
    Port 8080
    URI /api/v1/ingest/${container_name}/${HOSTNAME}
    Format json
    Header X-API-Key ${API_KEY}
    Retry_Limit 5
```

### Plain HTTP

Anything that can POST works; the format of each line is auto-detected (JSON, CLF, plain text):

```bash
curl -X POST "http://your-pooml-host:8080/api/v1/ingest/my-service/my-host" \
  -H "X-API-Key: $API_KEY" \
  -d '{"level":"error","message":"payment declined","order_id":42}'
```

## Shipping metrics

Pooml stores exactly two metric types: **counters** and **gauges**. Everything richer is normalized down to them - histograms and summaries become `<name>_sum` / `<name>_count` counter pairs (enough for rates, totals, and averages via SQL), and untyped Prometheus samples are treated as gauges. If you need native histogram buckets and quantiles, that's Prometheus territory.

**Push (OTLP/HTTP)**: point your OpenTelemetry SDK or collector at `http://your-pooml-host:8080/api/v1/otlp/v1/metrics` with the `X-API-Key` header. Both protobuf and JSON encodings are accepted.

**Pull (Prometheus scrape)**: add scrape targets in Settings - URL, interval, optional auth header. Pooml scrapes them and normalizes the samples into `metrics.db`.

The full HTTP API is documented in [openapi.yaml](./openapi.yaml).

## Query API (SQL over HTTP)

Opt-in, off by default: set `POOML_QUERY_API_ENABLED=true` and `POOML_QUERY_AUTH_SECRET` (its own secret - a leaked ingest key grants no read access). Then:

```bash
curl -X POST "http://your-pooml-host:8080/api/v1/query/logs" \
  -H "X-API-Key: $QUERY_SECRET" -H "Content-Type: application/json" \
  -d '{"sql": "SELECT service, COUNT(*) AS errors FROM logs WHERE level >= 4 GROUP BY service ORDER BY errors DESC"}'
```

`/api/v1/query/logs`, `/api/v1/query/metrics` (same layered SQL validation as the UI: SELECT-only, allow-listed tables, read-only connection, timeouts), and `GET /api/v1/query/catalog` for what metrics exist. Responses default to 200 rows (`max_rows` up to 1000) with long cells truncated - budgets sized for scripts and AI assistants. Details in [openapi.yaml](./openapi.yaml).

## MCP: ask your AI what broke

Because pooml's query language is SQL, your AI assistant already knows how to use it. [`@pooml/mcp`](https://www.npmjs.com/package/@pooml/mcp) is a small MCP server that runs on your machine and connects any MCP client (Claude Code, Claude Desktop, ...) to the query API above - strictly read-only, through the same validation stack, with your credentials never leaving your machine.

It rides on the query API, which is **disabled by default** - enable it on the pooml server first:

```
POOML_QUERY_API_ENABLED=true
POOML_QUERY_AUTH_SECRET=<min 32 chars, its own secret - not an ingest API key>
```

Then, on the machine where your MCP client runs:

```bash
claude mcp add pooml \
  -e POOML_URL=https://your-pooml-host:8080 \
  -e POOML_QUERY_AUTH_SECRET=your-query-secret \
  -- npx -y @pooml/mcp
```

Then ask things like *"what broke in payment-svc last night?"* or *"chart-worthy: which service's error rate spiked this week?"* - the model writes the SQL, pooml answers it. Setup details (including running behind a Cloudflare Tunnel) in the [pooml-mcp repo](https://github.com/n0rdy/pooml-mcp).

## Retention

Logs default to 30 days, metrics and alert history to 90 - all configurable in Settings (1 to 3650 days). Cleanup runs hourly in small batches so ingestion never stalls, and freed pages are reclaimed with incremental vacuum.

## Backups

Pooml backs up all three databases to any S3-compatible storage (AWS S3, Cloudflare R2, MinIO, Backblaze B2) on a cron schedule, using SQLite's online backup API - writes continue during the backup. Configure bucket, credentials, and schedule in Settings; credentials are encrypted at rest with `POOML_ENCRYPTION_KEY`.

Each run uploads to one timestamped folder, so a restore is "take all three files from one folder" - stop pooml, place them in `POOML_DB_DIR`, start pooml:

```
{bucket}/{prefix}/2026-04-27T030000Z/logs.db
{bucket}/{prefix}/2026-04-27T030000Z/metrics.db
{bucket}/{prefix}/2026-04-27T030000Z/meta.db
```

### Expiring old backups

Pooml never deletes backups and never needs delete permissions - even a fully compromised pooml cannot destroy your backup history. Expire old backups with your provider's lifecycle rules instead:

**AWS S3 / Cloudflare R2:**

```bash
aws s3api put-bucket-lifecycle-configuration --bucket my-backups \
  --lifecycle-configuration '{"Rules":[{"ID":"expire-pooml-backups","Status":"Enabled",
    "Filter":{"Prefix":"pooml/"},"Expiration":{"Days":30}}]}'
```

(For R2, add `--endpoint-url https://<account_id>.r2.cloudflarestorage.com`.)

**MinIO:**

```bash
mc ilm rule add --expire-days 30 --prefix "pooml/" myminio/my-backups
```

The minimum credentials policy pooml needs is `s3:PutObject` on the bucket.

## Monitoring pooml itself

Set `POOML_METRICS_ENABLED=true` and `POOML_METRICS_AUTH_SECRET`, and pooml exposes its own operational metrics at `:8080/metrics` (Prometheus text format) - and registers itself as a scrape target, so its own metrics land in `metrics.db` and are graphable in its own dashboards. No separate monitoring stack needed; external Prometheus can scrape the same endpoint with the secret in `X-API-Key`.

## Security notes

- Keep the UI (port 8081) off the public internet, or at least behind a reverse proxy with TLS. The ingestion API (8080) is what your services need to reach.
- Login and API-key failures are throttled per IP. If you run behind a reverse proxy, set `POOML_TRUST_PROXY_HEADERS=true` (and make sure the proxy overwrites `X-Forwarded-For`).
- Secrets configured in the UI are encrypted at rest; `logs.db` and `metrics.db` content is not. Backups inherit that, so enable server-side encryption on the bucket if you need encryption at rest.
- Scrape targets and the Campfire base URL are fetched server-side: anyone with UI admin access can point them at internal network addresses. That is consistent with pooml's single-admin model (the same admin already runs arbitrary SQL), but worth knowing if the pooml host sits inside a sensitive network.

## License

[AGPL-3.0](./LICENSE)
