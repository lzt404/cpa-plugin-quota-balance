# Quota Balance — CLIProxyAPI 调度插件

`round-robin` 和 `fill-first` 会把流量平均分给同一 provider 下的每一个凭证。当你有多个同源
OAuth 账号（Claude Pro/Max、Codex）时，这意味着一个 5 小时额度只剩 3% 的账号，会和一个满额
账号一样持续接量，直到它返回 `429`。

**Quota Balance** 在你启用的 provider 上接管调度。每次选凭证时，它读取每个候选的**真实剩余
额度**，优先选择余量最多的：

| 等级 | 剩余量（瓶颈窗口） | 处理方式 |
| --- | --- | --- |
| healthy | ≥ `low_threshold`（默认 15%） | 优先，剩余越多越靠前 |
| unknown | 没有额度数据（接口失败、无 token） | 排在 healthy 之后、low 之前 |
| low | `critical_threshold` … `low_threshold` | 排在健康账号之后 |
| critical | < `critical_threshold`（默认 5%） | 仅作最后兜底 |

和一次性改写 `priority`/`weight` 不同，它在两次刷新之间依然有效：额度是实时探测的，按
`cache_ttl` 缓存，五分钟前刚被打空的账号不会再被优先选中。配置的 `priority` 仍用于同级排序，
所以额度相同或不可用时，现有顺序原样保留。

## 它不做什么

- 不修改 auth 文件，`priority`/`weight` 保持原样。
- 不参与模型路由与会话亲和决策，只回答「选哪个凭证」。
- 不伪造额度。用法接口不可达时所有凭证都是 `unknown`，回落到宿主优先级顺序。

## 要求

- 带 `scheduler`、`management_api` 插件能力，以及 `host.auth.list`、`host.auth.get`、
  `host.http.do` 宿主回调的 CLIProxyAPI v8。`v8.0.59` 已发布（`go.mod` 已固定该 SDK）。
- 支持的 provider：`claude`（`https://api.anthropic.com/api/oauth/usage`）和
  `codex`（`https://chatgpt.com/backend-api/wham/usage`）。

## 安装

1. 把对应平台的发布产物放进核心插件目录：`plugins/quota-balance.so`（Linux）或
   `plugins/quota-balance.dylib`（macOS）。
2. 在 `config.yaml` 中启用：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    quota-balance:
      enabled: true
      priority: 0            # 任意值，插件只接管调度
      cache_ttl: 5m          # 一次额度观测的有效期
      refresh_interval: 2m   # 后台重新探测所有凭证的周期
      timeout: 8s            # 单个凭证探测超时
      low_threshold: 0.15
      critical_threshold: 0.05
      quota_providers: ["claude", "codex"]
      ignore_providers: []   # 例如 ["codex"]，这些 provider 交回内置调度
      claude_usage_endpoint: ""  # 自建镜像/网关可覆盖
      codex_usage_endpoint: ""
```

3. 重启核心。`GET /v0/management/plugins/quota-balance/status` 会返回每个凭证的额度观测和
   最近的调度决策。

## 打分方式

后台只有一个探测循环，按 `refresh_interval` 重新探测所有凭证；**调度本身从不等待网络**，
所以额度接口变慢或失败不会给模型请求增加延迟。观测时间超过 `cache_ttl` 的凭证按 `unknown` 处理。

每次探测通过 `host.auth.get` 读取凭证的 `access_token`，再通过 `host.http.do` 调用
provider 的额度接口，取**瓶颈窗口**——所有 bucket 中最小的剩余比例，这样 5 小时的余量就掩盖
不了一个快用完的 7 天窗口。Anthropic 的「已用百分比」（`42` 表示 42%）会换算成剩余比例
（`0.58`）；Codex 的 `limit_reached` 直接记为 0。

排序依据依次是：等级 → 剩余比例 → 宿主 `priority` → 宿主候选顺序。

## 已验证行为

`go test -race ./...` 覆盖等级阈值、瓶颈打分、并发去重探测、缓存过期、过期观测、忽略/允许
列表、未知额度回落、provider 报文解析与配置校验。

`acceptance.py` 使用真实的 `v8.0.59` 核心、三个真实 Claude auth 文件、本地伪 Anthropic
接口（额度端点 + `/v1/messages`），对**已发布的 `.so`** 实际做出的调度决策断言：

| 检查项 | 结果 |
| --- | --- |
| 健康凭证优先于优先级更高的低额度凭证 | `acct-alpha` |
| 额度变化后改选恢复的凭证 | `acct-beta` |
| 多数凭证 critical 时选择剩余最多的 | `acct-gamma` |
| 管理接口列出已观测凭证 | 3 个凭证 |
| 插件禁用后不再暴露调度接口 | HTTP 404 |

无额度回落、观测过期处理和「调度不同步探测」由竞态单元测试覆盖，不需要真实核心。

## 许可证

MIT
