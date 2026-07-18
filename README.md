# silo-plugin-wisp

A thin [Silo](https://github.com/Silo-Server) `request_router.v1` plugin that
delegates all fulfillment to a [Wisp](https://github.com/dreulavelle/wisp)
server's HTTP API.

The plugin holds no business logic. Every request Silo routes to it is handed
straight to Wisp (`POST /api/add`); Wisp owns resolution, quality pinning, and
status. This shim only translates between Silo's gRPC `request_router` contract
and Wisp's HTTP API. It replaces the retired aiostreams plugin's "wisp mode".

## How it works

- **Fulfill** makes a single `POST /api/add` call and returns immediately. Wisp's
  intake is asynchronous and idempotent — a `202` means "accepted and now
  monitored", not "downloaded". Every requested quality comes back as a `queued`
  target. The call uses a 10s client timeout, so fulfillment finishes far inside
  Silo's 60s deadline (the old wisp mode resolved synchronously and could blow
  the deadline — this one never does).
- **CheckStatus** polls `GET /api/requests/status` and maps Wisp's per-title
  state onto each target (table below). Targets are grouped by connection, so
  Wisp is queried at most once per connection per check.
- **TestConnection** hits `GET /api/healthz`.
- **Validate** checks that `wisp_url` parses as an `http(s)` URL. No network.
- **ListConfigOptions** returns nothing — the form has no dynamic dropdowns.

## Install into Silo

1. Build (or download) the plugin binary for your platform. From source:
   ```sh
   make build          # ./silo-plugin-wisp for the host platform
   make dist           # dist/silo-plugin-wisp-linux-{amd64,arm64}
   ```
   `make manifest` prints the manifest with the binary's checksum stamped in,
   which is what Silo reads on install.
2. In Silo, go to **Admin → Plugins** and install the binary, or point Silo's
   plugin-repository setting at this project's `repository.json`. Use the raw
   `main` URL, which the release workflow keeps in sync with the latest release:
   ```
   https://raw.githubusercontent.com/dreulavelle/silo-plugin-wisp/main/repository.json
   ```
   (Each release also attaches a `repository.json` asset pinned to that tag.)
3. Add a connection for the **Wisp Requests** capability and fill in the
   connection config below.
4. Click **Test Connection** to confirm Silo can reach Wisp.

## Connection config

The capability exposes one connection config schema, keyed `connection`:

| Field        | Required | Control  | Purpose |
|--------------|----------|----------|---------|
| `wisp_url`   | yes      | text     | Base URL of the Wisp server, e.g. `http://wisp:8080`. |
| `wisp_token` | no       | password | Optional token. When set it is sent to Wisp as `Authorization: Bearer <token>`. Leave empty if Wisp needs no auth. |

These values arrive on each call inside `RouterConnection.config` (Silo's admin
form delivers custom fields in the config `Struct`, keyed by field name). As a
forward-compat fallback the plugin also reads the standardized
`RouterConnection.base_url` / `api_key` slots if `config` is empty.

**One connection per installation.** This plugin fronts exactly one Wisp server.
Configure a single connection for the capability. If more than one is present,
`Fulfill` returns zero targets with an explanatory message rather than fanning a
request out across backends — remove the extras.

## Status mapping

`CheckStatus` translates Wisp's request state into Silo's host-normalized
statuses per target:

| Wisp response                     | Target status | Notes |
|-----------------------------------|---------------|-------|
| `state: "completed"`, quality ∈ `pinned_qualities` | `completed` | The requested tier is pinned and servable. |
| `state: "completed"`, quality ∉ `pinned_qualities` | `queued`    | Title done, but this tier is not pinned yet. |
| `state: "queued"`                 | `queued`      | Tracked; nothing in scope pinned yet (incl. unreleased). |
| `state: "failed"`                 | `failed`      | Permanent give-up; Wisp's `detail` is passed through as the message. |
| `404` (untracked)                 | `queued`      | Wisp isn't tracking it yet; the host will re-poll and/or re-submit. |
| transport / non-404 HTTP error    | `queued`      | Treated as transient so the host keeps polling. |

`Fulfill` responses always start every target at `queued`. On a missing
connection, missing `wisp_url`, missing identity (no tmdb/imdb id), or any Wisp
failure, `Fulfill` returns **zero targets plus a top-level message** — Silo
treats zero targets as a submission failure and retries.

## Design rationale

All fulfillment logic lives in Wisp behind a stable HTTP API. Keeping it there —
rather than in a Silo plugin — means Silo SDK churn (proto changes, runtime
changes, new capability revisions) only ever touches this ~300-line shim, never
the resolution, pinning, or monitoring logic. The plugin is deliberately small,
stateless, and holds no credentials: connections are passed in on every call.

## Development

```sh
make all      # fmt-check + vet + test + build
make test     # go test ./...
```

Tests use an `httptest` Wisp stub and drive the gRPC-facing handlers directly
(constructing `pluginproto` messages) — no live Silo or Wisp needed.
