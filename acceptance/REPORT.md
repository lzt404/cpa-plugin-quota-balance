# Acceptance report — quota-balance 1.0.0

Executed against the **released core binary** built from tag `v8.0.59`
(`CGO_ENABLED=1 go build ./cmd/server`), not a patched worktree.

- Core SHA-256: `f9c123ebf0a1fd29d91c1ae53ad2c49cbea4ebb308bf69abea7e2df8c86f3359`
- Plugin SHA-256: `b11936c7b9be472f23863c29f785dfc23d107147686b0ccde10ecf56f0da31d6`
- Release asset SHA-256 (linux/amd64 zip): `a46d61ed3ceead5d1e7db04f4a2b47a71123ad951163c15db72ea43057e40d78`
- Shipped `quota-balance.so` SHA-256: `a20fe8c8ca841067add94dbce724c3622d6406a6836f57bb0de695cd1b936154`
- Acceptance below was re-run against the **downloaded release asset**, not the local build
- Plugin loaded from `dist/1.0.0-linux-amd64/quota-balance.so` (the packaged artifact)
- Harness: `acceptance.py` (this directory). Fixture executor lives in `acceptance/fixture/`
  and is **not** part of the store archive.

## Method

Three real Claude auth files (`acct-alpha` priority 1, `acct-beta` priority 2,
`acct-gamma` priority 3) point at a local fake Anthropic surface that serves:

- `GET /usage` — per-token `five_hour`/`seven_day` utilization, mutated between phases.
- `POST /v1/messages` — echoes the serving credential's token, proving which credential ran.

Quota is read by the plugin through the real `host.auth.get` then `host.http.do` path.
Assertions read `/v0/management/plugins/quota-balance/status`, which reports every scheduling
decision the plugin actually returned.

Because the host retries a failed credential inside one request, only the first pick of a
request sees the full candidate set; the harness asserts on fully-ranked picks.

## Results

| Check | Observed |
| --- | --- |
| healthy credential preferred regardless of priority | acct-alpha |
| quota change re-routes to the recovered credential | acct-beta |
| least-drained credential wins when others are critical | acct-gamma |
| missing quota evidence falls back to priority order | acct-alpha |
| management status lists observed credentials | 3 |
| disabled plugin exposes no scheduler status route | 404 |
| core starts | 2 |

Race-enabled unit suite: `go test -race ./...` reports `ok` (16 tests).

## Gaps

- linux/arm64 and darwin/* assets are produced only by the release workflow; they were not
  executed on this host (no aarch64 cross toolchain installed).
- The upstream Anthropic API is unreachable from the sandbox, so `/v1/messages` is served by
  the harness; quota parsing used the documented `utilization` field shape, validated in unit
  tests against real payload samples.
