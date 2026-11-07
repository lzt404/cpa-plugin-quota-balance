# Acceptance report — quota-balance 1.1.0

Executed against the **released core binary** built from tag `v8.0.59`
(`CGO_ENABLED=1 go build ./cmd/server`), not a patched worktree.

- Core SHA-256: `f9c123ebf0a1fd29d91c1ae53ad2c49cbea4ebb308bf69abea7e2df8c86f3359`
- Plugin SHA-256: `2e008a60a812c3b4e5a5ee5e9f4a48ab5013ab7b0ea6f01eb171c7b95f95ccdb` (extracted from the downloaded linux/amd64 release zip)
- Plugin loaded from `dist/1.1.0-linux-amd64/quota-balance.so` (the packaged artifact)
- Harness: `acceptance.py` (repository root). The fixture executor in `acceptance/fixture/`
  is a test-only helper and is **not** part of the store archive.

## Method

Three real Claude auth files (`acct-alpha` priority 1, `acct-beta` priority 2,
`acct-gamma` priority 3) point at a local fake Anthropic surface serving:

- `GET /usage` — per-token `five_hour`/`seven_day` utilization, mutated between phases.
- `POST /v1/messages` — answers so a request completes end to end.

Quota is read by the released `.so` through the real `host.auth.get` then `host.http.do` path
in a background refresher (`refresh_interval: 0.4s` for the test). Assertions read
`/v0/management/plugins/quota-balance/status`, which reports each observed quota and every
scheduling decision the plugin returned.

The host retries a failed credential inside one request, so only the first pick of a request
sees the full candidate set; the harness asserts on that first pick.

The harness waits for exact observed values before each phase, so it cannot race the refresher.

## Results

| Check | Observed |
| --- | --- |
| healthy credential preferred regardless of priority | acct-alpha |
| quota change re-routes to the recovered credential | acct-beta |
| least-drained credential wins when others are critical | acct-gamma |
| management status lists observed credentials | 3 |
| disabled plugin exposes no scheduler status route | 404 |
| core starts | 2 |

Race-enabled unit suite: `go test -race ./...` reports `ok`.

## Deliberately not in the live matrix

These are asserted by the unit suite instead, because they need no live core and the live
phases involve real network timeouts:

- unknown quota (every usage lookup failing) falls back to host priority order
- an observation older than `cache_ttl` ranks as unknown
- `scheduler.pick` performs no synchronous probe

## Gaps

- linux/arm64 and darwin/arm64 assets are produced by the release workflow; they were built by
  CI but not executed on this host (no aarch64 cross toolchain installed locally).
- The upstream Anthropic API is unreachable from the sandbox, so `/v1/messages` is served by
  the harness; quota parsing uses the documented `utilization` field shape and is validated in
  unit tests.
