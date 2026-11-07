# Quota Balance — a CLIProxyAPI scheduler plugin

`round-robin` and `fill-first` spread traffic evenly across every credential of a provider.
With several OAuth accounts of the same provider (Claude Pro/Max, Codex) that means an account
sitting at 3% of its 5-hour window keeps receiving exactly as much traffic as a fresh one until
it answers `429`.

**Quota Balance** replaces the scheduler for the providers you enable it for. On every pick it
reads each candidate's *actual* remaining quota and prefers the one with the most headroom:

| Tier | Remaining (bottleneck window) | Treatment |
| --- | --- | --- |
| healthy | ≥ `low_threshold` (default 15%) | preferred, highest remainder first |
| unknown | no quota evidence (endpoint failed, no token) | used after healthy, before low |
| low | `critical_threshold` … `low_threshold` | demoted behind healthy accounts |
| critical | < `critical_threshold` (default 5%) | last resort only |

Unlike a one-shot rewrite of `priority`/`weight`, it keeps working between refreshes: quota is
probed live, cached for `cache_ttl`, and a credential drained five minutes ago is no longer
preferred. The configured `priority` still breaks ties, so existing ordering is preserved when
quota is equal or unavailable.

## What it does not do

- It does not modify auth files. `priority`/`weight` stay exactly as configured.
- It does not decide model routing or session affinity. It only answers "which credential".
- It does not fabricate quota. Without a reachable usage endpoint every credential stays
  `unknown` and the host priority order applies.

## Requirements

- CLIProxyAPI v8 with the `scheduler` and `management_api` plugin capabilities, plus the
  `host.auth.list`, `host.auth.get` and `host.http.do` host callbacks. Released in
  `v8.0.59` (SDK pinned in `go.mod`).
- Supported providers: `claude` (`https://api.anthropic.com/api/oauth/usage`) and
  `codex` (`https://chatgpt.com/backend-api/wham/usage`).

## Install

1. Copy the release asset into the core plugin directory:
   `plugins/quota-balance.so` (Linux) or `plugins/quota-balance.dylib` (macOS).
2. Enable it in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    quota-balance:
      enabled: true
      priority: 0            # any value; the plugin only handles scheduling
      cache_ttl: 5m          # how long an observation ranks a credential
      timeout: 8s            # per-credential probe timeout
      low_threshold: 0.15
      critical_threshold: 0.05
      quota_providers: ["claude", "codex"]
      ignore_providers: []   # e.g. ["codex"] to leave those to the built-in scheduler
      claude_usage_endpoint: ""  # override for a mirror/gateway
      codex_usage_endpoint: ""
```

3. Restart the core. `GET /v0/management/plugins/quota-balance/status` reports the observed
   quota per credential and the most recent scheduling decisions.

## How the score is computed

Each probe reads the credential's `access_token` through `host.auth.get`, calls the provider
usage endpoint through `host.http.do`, and keeps the **bottleneck** window — the smallest
remaining fraction across every bucket, so 5-hour headroom cannot hide an exhausted 7-day
window. Anthropic's "percent used" (`42` means 42%) is converted to a remaining fraction
(`0.58`); Codex `limit_reached` short-circuits to zero.

Requests are ranked by tier → remaining fraction → host `priority` → host candidate order.

## Verified behaviour

`go test -race ./...` covers tier thresholds, bottleneck scoring, deduplicated probing, cache
expiry, ignore/allow lists, unknown-quota fallback, provider payload parsing and config
validation.

`acceptance.py` runs a real `v8.0.59` core binary with three real Claude auth files, a local
fake Anthropic surface (usage endpoint + `/v1/messages`), and asserts on the scheduling
decisions the released `.so` actually made:

| Check | Result |
| --- | --- |
| healthy credential preferred regardless of priority | `acct-alpha` |
| quota change re-routes to the recovered credential | `acct-beta` |
| least-drained credential wins when others are critical | `acct-gamma` |
| missing quota evidence falls back to priority order | `acct-alpha` |
| management status lists observed credentials | 3 credentials |
| disabled plugin exposes no scheduler route | HTTP 404 |

## License

MIT
