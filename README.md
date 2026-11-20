# CPA Quota

CPA Quota 是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 的插件（Go 编写的 c-shared 动态库），只做三件事：

1. **额度估算**：根据经插件计价的请求费用和响应头报告的 5 小时 / 7 天利用率，估算订阅账号（Claude Pro / Max 5x / Max 20x，ChatGPT Team）的 5 小时与 7 天总额和剩余额度（USD）。
2. **额度接口**：通过带 token 的只读 HTTP 接口提供估算结果，同时写入 PostgreSQL 供下游直接查询。
3. **记录 safeguard 事件**：Claude 以安全策略拒绝请求时记一行。

API key、套餐、价格表编辑、请求拦截 / 放行、路由、凭证管理、自助页和管理页 UI 都不在本插件范围内，由下游自建。插件不修改任何请求或响应。

## 部署形态

- 每个 CPA 实例只挂一个订阅账号，需要多个账号就多开 CPA。
- 所有实例的插件共用同一个 PostgreSQL，每一行都带 `instance` 列，按 `instance` + 账号 `auth_index` 区分。
- 插件不起自己的 goroutine 或定时器：刷新、重连、补写都在 `usage.handle` 或接口请求中同步完成，所有网络操作都有短超时（数据库默认 3 秒，倍率表默认 5 秒）。

## 安装

插件 ID 由动态库文件名决定，必须命名为 `cpa-quota.so`（macOS 为 `.dylib`，Windows 为 `.dll`）并放进 CPA 的 `plugins.dir`。

```bash
CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -o cpa-quota.so ./cmd/cpa-quota
```

发版后也可以通过 `plugins.store-sources` 指向本仓库的 `registry.json` 从插件商店安装（release 产物为 `cpa-quota_<版本>_<os>_<arch>.zip` 加 `checksums.txt`）。

## 配置

全部配置写在 CPA `config.yaml` 的 `plugins.configs.cpa-quota` 下，没有 UI。

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-quota:
      enabled: true
      instance: "cc-tokyo-1"                 # 必填
      postgres_dsn: "postgres://cpa:secret@db.internal:5432/quota?sslmode=require"   # 必填
      api_token: "change-me-read-only"
      admin_token: "change-me-admin"
      subscription: claude                   # claude | chatgpt
      tier: max5x                            # claude: pro | max5x | max20x；chatgpt: team
      # manual_k: 6.5
      # safety_factor: 0.9
      # model_weights:
      #   enabled: false
      #   items:
      #     - id: fable
      #       name: Claude Fable
      #       models: [claude-fable-5-2]
```

| 键 | 默认值 | 说明 |
| --- | --- | --- |
| `instance` | 必填 | 实例名，最长 128 字节；写进每一行数据和接口返回。 |
| `postgres_dsn` | 必填 | PostgreSQL 连接串（pgx 格式，URL 或 key=value）。 |
| `postgres_schema` | `cpa_quota` | 插件的表所在 schema，启动时自动创建。 |
| `api_token` | 空 | 额度接口的只读 Bearer token；为空时额度接口一律 401。 |
| `admin_token` | 空 | 管理接口的 Bearer token，与 `api_token` 分开；为空时管理接口禁用。 |
| `price_url` | `https://toll.icmp.day/api/ratio_config` | new-api 格式的倍率表地址。 |
| `price_refresh_hours` | `24` | 倍率表超过这个时长后，下一次 `usage.handle` 同步刷新。 |
| `price_retry_minutes` | `5` | 刷新失败后的最短重试间隔，避免每条用量都去请求。 |
| `price_timeout_seconds` | `5` | 拉取倍率表的超时。 |
| `subscription` | `claude` | 订阅类型：`claude` 或 `chatgpt`（CPA 的 Codex 通道）。 |
| `tier` | 订阅的第一档 | Claude：`pro`（默认）/ `max5x` / `max20x`；ChatGPT：`team`。 |
| `manual_k` | 无 | 手动指定周额度 / 5 小时额度比值（1–100），优先于估算值。 |
| `safety_factor` | `0.9` | 安全系数，剩余额度 R = 系数 × T × (1 − u)。 |
| `tier_weekly_usd` | Claude `pro: 250, max5x: 1250, max20x: 2500`；ChatGPT `team: 60` | 冷启动时使用的档位周总额，可按档位覆盖。 |
| `tier_k` | Claude 各档 `8`；ChatGPT 无 | 没有手动和估算 k 时使用的档位默认 k，可按档位覆盖。 |
| `auth_indexes` | 空 | 非空时只统计这些账号，并且启动即列出它们；为空时自动跟踪出现的同订阅账号。 |
| `account_auth_types` | `[oauth]` | 计入的凭证类型（CPA 的 `AuthType`：`oauth`、`apikey`）。 |
| `codex_fast_mode_billing` | `false` | 开启后 ChatGPT `priority` 请求（按客户端请求的 service tier 判断）按 2.5 倍计费。 |
| `db_timeout_seconds` | `3` | 连接与每次数据库操作的超时。 |
| `db_retry_seconds` | `60` | 数据库不可用后的最短重连间隔。 |
| `refresh_interval_seconds` | `60` | 估算刷新的最短间隔。 |
| `process_budget_seconds` | `3` | 每次刷新最多处理多长时间的历史积压，余下的留给后续刷新。 |
| `rebuild_budget_seconds` | `20` | 「从历史重建」一次请求内的处理时长上限。 |
| `trend_timezone` | `UTC` | 每日 k 趋势按哪个时区切天（IANA 名称）。 |
| `forecast_window_5h_minutes` | `60` | 5 小时窗口消耗速度的回看时长（5–300）。 |
| `forecast_window_7d_hours` | `24` | 7 天窗口消耗速度的回看时长（1–168）。 |
| `estimation.segment_retention_days` | `365` | 段与 k 证据的保留天数。 |
| `estimation.block_gap_minutes` | `5` | 相邻数据点间隔超过它就断开大块。 |
| `estimation.window_percent` | `8` | 污染过滤滑动窗口宽度（利用率点数）。 |
| `estimation.reference_mode` | `global` | 参考比值：`global`（历史加权）或 `block`（块内迭代）。 |
| `estimation.suspicious_threshold` | `0.2` | 窗口比值超过参考 20% 视为可疑。 |
| `estimation.exclude_vote_share` | `0.5` | 覆盖某段的窗口中可疑占比达到它就剔除该段。 |
| `estimation.weight_power` | `2` | 跨大块加权的指数（按干净增量的幂）。 |
| `estimation.recent_days` | `14` | 近期 / 远期分界。 |
| `estimation.recent_share` | `0.9` | 近期组的权重。 |
| `estimation.k_min_weekly_points` | `10` | 估算 k 需要的最少 7 天增量。 |
| `estimation.cold_start_blocks` | `20` | 干净大块少于它时处于冷启动，用档位默认周总额。 |
| `model_weights.enabled` | `false` | 估算是否使用模型额度倍率换算成本。 |
| `model_weights.items` | `[]` | 待测模型：`id`、`name`、`models`（计费模型名）、`manual_multiplier`。 |
| `model_weights.min_segments` / `min_cost_usd` / `min_share_spread` | `30` / `20` / `0.05` | 倍率估算的最少样本条件。 |

配置不合法（缺 `instance` / `postgres_dsn`、档位不属于订阅等）时插件注册失败并在 CPA 日志中给出原因。

## 估算方法

估算核心在 `internal/ccbilling`：

- 每次利用率跨过一个整数点切出一段（1% 段），记录这段期间完成的请求费用；百分比带小数时按跨整数点切段。
- 主窗口内的请求只要费用没能算出来（模型未定价，或用量不完整、不可计费），它完成时所在的段就作废：不写入 `segments`，不参与总额估算和模型倍率估算 / 校准，大块照常延续（作废段的增量和费用一起剔除，不影响相邻段的滑动窗口）。作废段按原因计数（目前只有 `unpriced`），见下文「费用缺失的请求」。
- 相邻数据点间隔超过 5 分钟断开大块；用 8% 宽的滑动窗口比较「点数 / 美元」，剔除可疑（被外部用量污染）的段。
- 各大块的干净比值按干净增量的平方加权，近 14 天占 90%，得到 5 小时总额 T5 = 100 / 比值。
- k（周 / 5 小时）只由两窗口同期的利用率增量估算，不用段和费用，因此不受费用缺失影响；周总额 T7 = k × T5；手动 k 优先，其次估算值，最后档位默认值。
- 干净大块少于 20 时冷启动，周总额取档位默认值。
- 剩余额度 R = 0.9 × T × (1 − u)，u 为当前窗口利用率。
- Claude 读 `anthropic-ratelimit-unified-5h/7d-*`；ChatGPT 读 `X-Codex-Primary/Secondary-*`，只把主额度（premium）的 5 小时 / 周窗口计入，额度用满或在扣 credits 时暂停切段；只有 7 天窗口的账号按 7 天窗口切段。
- 另外计算模型倍率（按各段的模型费用拆分估算，或按校准时段测量）、两个窗口的消耗预测（回看 `forecast_window_*`）和每日 k 趋势（按 `trend_timezone` 切天）。

## 价格

从 `price_url` 拉取 new-api 格式倍率表 `{"success":true,"data":{"model_ratio":{},"completion_ratio":{},"cache_ratio":{},"create_cache_ratio":{}}}`，换算为 USD / 百万 token：

- 输入 = `model_ratio` × 2
- 输出 = 输入 × `completion_ratio`
- 缓存读 = 输入 × `cache_ratio`
- 缓存写 = 输入 × `create_cache_ratio`

某个比率缺失时按 1 处理，即该部分按输入单价计费。表里没有的模型视为未定价，`requests.cost_usd` 记 0、`priced` 为 false。

模型按「上游模型名 → 客户端别名」的顺序查找，忽略大小写、`(high)` 之类的请求选项、`/` 之前的命名空间和 `-high` / `-low` 后缀。token 按 CPA 对 Claude（输入、缓存读写分列，输出已含推理）和 Codex（缓存含在输入里，推理含在输出里）的口径拆分，推理不重复计费；计数自相矛盾或只有总数的用量（`accounting` 为 `inconsistent` / `unclassified`）记录但不计费，`cost_usd` 记 0。

### 费用缺失的请求

记 0 的费用不是真实费用，把它算进段会让段显得偏便宜、周总额被低估，所以对所有订阅一视同仁：

- 主窗口内的请求，模型未定价且报告了用量，或者用量不完整（`inconsistent` / `unclassified`），它完成时所在的段作废，原因记为 `unpriced`。没有报告任何用量的请求（如失败请求）费用确实为 0，不导致作废。
- 落在大块第一次跨点之前的请求本来就不属于任何段，不作废任何段。
- ChatGPT 中属于另一个额度池的请求（响应没有主额度窗口，原因 `other_limit`，包括没有额度报告的请求）照旧只剔除自身费用，段不作废，即使它的模型未定价。
- 诊断：`streams.state` 的 `excluded` 中，`requests` / `usd` 按原因统计被剔除的请求数与费用，`unpriced_models` 列出未定价模型及其请求数（最多 50 个，Claude 和 ChatGPT 都会列出），`voided_segments` 按原因统计作废段数；额度接口的 `estimate.voided_segments` 给出作废段总数。

刷新：注册时拉一次；之后 `usage.handle` 发现距上次成功超过 24 小时就同步拉一次（失败沿用旧表并写日志，5 分钟内不重试）。最近一次成功的倍率表存进 PostgreSQL；启动时拉取失败就用库里的。

## PostgreSQL

插件启动时在 `postgres_schema` 中自动建表和迁移。迁移在事务内先取 advisory lock（`pg_advisory_xact_lock(0x6370612d71756f74)`），多个实例同时启动时串行执行，版本记在 `meta.schema_version`（当前为 1）。数据库版本高于插件时拒绝运行。

事件时间以 Unix 纳秒（`*_ns`，bigint）保存，保证重建估算看到与实时估算完全相同的时刻；每个 `*_ns` 列都有一个同名去掉后缀的 `timestamptz` 生成列方便阅读（0 表示未知，对应 NULL）。

### 事实表（`usage.handle` 写入）

**`requests`**：每个请求的精简记录，「从历史重建」以它为准。按 `(instance, request_id)` 去重。

| 列 | 说明 |
| --- | --- |
| `instance`, `auth_index`, `provider` | 实例、账号、`claude` / `codex` |
| `request_id` | CPA 的 RequestID |
| `requested_ns` / `requested_at`, `latency_ms`, `completed_ns` | 请求开始时间、耗时、完成时间（费用在完成时计入） |
| `model`, `upstream_model`, `service_tier`, `stream` | 计费模型名（别名优先）、上游模型名、请求的 service tier、是否流式 |
| `input_tokens`, `cache_read_tokens`, `cache_write_tokens`, `output_tokens`, `reasoning_tokens`, `total_tokens` | 计价口径的 token：未缓存输入、缓存读、缓存写、计费输出（含推理）、其中推理、总数 |
| `accounting` | `complete` / `inconsistent` / `unclassified`，空表示未报告用量；只有 `complete` 计费 |
| `cost_usd`, `priced` | 费用（USD）、模型是否在倍率表里 |
| `main_window` | 本请求的响应是否报告了主额度窗口（ChatGPT 只计入为 true 的请求） |
| `failed`, `status_code` | 是否失败、失败状态码 |

**`quota_reports`**：每个带额度响应头的请求一行：`instance`、`auth_index`、`provider`、`request_id`、`at_ns`（请求开始时间）、`plan`、`credit_balance`、`credits_unlimited`、`limit_reached`。

**`quota_report_windows`**：报告中的每个窗口：`report_id`、`instance`、`auth_index`、`provider`、`limit_key`（Claude `unified`；ChatGPT 主额度 `codex`、代码审查 `code-review`、附加额度用其名称）、`seconds`（18000 / 604800 等）、`at_ns`、`used_percent`、`reset_ns` / `reset_at`。

**`safeguard_events`**：见下文。

**`price_tables`**：每个实例最近一次成功拉取的倍率表：`instance`、`source_url`、`fetched_at`、`models`、`body`（原始 JSON）。

### 估算表（每次刷新写入）

| 表 | 主键 | 内容 |
| --- | --- | --- |
| `streams` | `instance, auth_index` | 增量处理位置（JSON）：游标、窗口跟踪、当前大块（含待作废标记）、暂停状态、`excluded`（被剔除的请求与费用、未定价模型、作废段计数） |
| `segments` | `id` | 计入估算的 1% 段（作废段不写入）：`block_ns`（所属大块起点）、`at_ns`、`cost_usd`、`gain`（利用率增量）、`models`（按模型拆分的费用 JSON） |
| `k_buckets` | `instance, auth_index, bucket_ns` | 每小时的 k 证据：`gain_5h`、`gain_7d` |
| `blocks` | `instance, auth_index, start_at` | 当前估算的大块统计：段数、剔除数、增量 / 费用、干净增量 / 费用、参考比值及模式、是否干净 |
| `estimates` | `instance, auth_index` | **当前估算快照，下游面板直接读这张**：列与额度接口账号对象的主要字段一致，`detail` 为完整账号对象 JSON |
| `k_trend_daily` | `instance, auth_index, day` | 每日 k 趋势：`gain_5h`、`gain_7d`、当日 `k`、保留段数、剔除段数、干净段费用与增量 |
| `model_weights` | `instance, provider` | 模型倍率结果：是否启用、`basis`、有效倍率、历史估算、各模型费用占比 |
| `calibrations` | `id` | 模型倍率校准：`item`、`auth_index`、起止时间、`result` |

### 写失败与数据库不可用

- 写入失败的事实按顺序暂存在内存，最多 10,000 条，超出丢弃最旧的并写日志；下次写入时先补写这些再写新的。数据库以「数据无效」拒绝的写入直接丢弃，不阻塞队列。CPA 重启时内存里的待写数据会丢失。
- 启动时连不上数据库：插件照常注册，账号按冷启动处理（档位默认周总额 + 响应头的实时利用率），接口中 `database.status` 为 `unavailable`。之后每分钟最多重连一次（在 `usage.handle` 或接口请求中）；连上后先按顺序补写内存里的数据，再从数据库加载历史、重建估算状态。
- 有待写数据时估算暂停推进，避免游标越过尚未落库的请求。

## 额度接口

```
GET /v0/resource/plugins/cpa-quota/quota
Authorization: Bearer <api_token>
```

CPA v8 的插件资源路由只支持 GET 且不做 CPA 自身的鉴权，所以路径在 `/v0/resource/plugins/<插件 ID>/` 下，由插件自己校验 Bearer token。token 缺失或错误返回 401 和 `{"error":{"code":"unauthorized","message":"..."}}`。接口每分钟最多触发一次估算刷新，其余时间只读内存。

### 返回字段（对下游的契约）

| 字段 | 说明 |
| --- | --- |
| `plugin`, `version`, `instance`, `generated_at` | 插件 ID、版本、实例名、生成时间 |
| `subscription`, `tier` | 配置的订阅类型与档位 |
| `price_table` | `source_url`、`origin`（`remote` 本进程拉取 / `database` 库里的 / `none`）、`updated_at`（倍率表拉取时间）、`models`、`last_attempt_at`、`last_error` |
| `database` | `status`（`ok` / `unavailable`）、`last_error`、`failed_at`、`pending_writes`、`dropped_writes`、`schema_version` |
| `estimates` | `loaded`（是否已有基于数据库的估算）、`estimated_at`、`last_error` |
| `accounts[]` | 每个账号一项（正常只有一个），字段如下 |

账号对象：

| 字段 | 说明 |
| --- | --- |
| `auth_index`, `provider`, `subscription`, `tier` | 账号、`claude` / `codex`、`claude` / `chatgpt`、档位 |
| `state` | `cold`（冷启动）/ `estimated`（已估算）/ `paused`（额度用满或扣 credits，切段暂停） |
| `pause_reason`, `paused_since` | `limit_reached` / `credits` 及开始时间，仅 `paused` 时出现 |
| `k`, `k_source`, `k_auto` | 生效的 k；来源 `manual` / `auto` / `default`（档位默认）/ `none`；估算出的 k |
| `segment_window` | 切段测量的窗口：`5h`，或没有 5 小时窗口的账号为 `7d` |
| `totals_from` | 总额来源：`estimated` 或 `tier`（档位默认） |
| `safety_factor` | 安全系数 |
| `observed_at` | 最近一次额度报告的时间 |
| `five_hour`, `week` | 窗口：`used_percent`、`reset_at`、`total_usd`（0 表示未知）、`remaining_usd`（已乘安全系数）、`forecast` |
| `forecast` | `source`（`reading` 按利用率读数 / `spend` 按费用 / `none`）、`per_hour_percent`、`outcome`（`eta` 窗口内耗尽 / `after_reset` 重置前不会耗尽 / `idle` / `reached` / `unknown`）、`exhausts_at`、`lookback_seconds` |
| `estimate` | `ratio`（点 / 美元）、`usd_per_point`、`clean_blocks`、`total_blocks`、`cold_start_blocks`、`segments`（计入估算的段）、`excluded_segments`（被污染过滤剔除的段）、`voided_segments`（因费用缺失作废的段，不在 `segments` 内）、`basis`（`raw` / `weighted`）、`estimated_at` |
| `model_weights` | `enabled`、`basis`、`suggestions[]`：`id`、`name`、`models`、`multiplier`（生效倍率）、`source`（`manual` / `calibration` / `estimated` / `default`）、`status`、`estimated_multiplier`、`low`、`high`（95% 区间）、`calibration_id` |

### 示例

```json
{
  "plugin": "cpa-quota",
  "version": "0.1.0",
  "instance": "cc-1",
  "generated_at": "2026-10-05T10:01:00Z",
  "subscription": "claude",
  "tier": "max5x",
  "price_table": {
    "source_url": "https://toll.icmp.day/api/ratio_config",
    "origin": "remote",
    "updated_at": "2026-10-05T10:00:00Z",
    "models": 54,
    "last_attempt_at": "2026-10-05T10:00:00Z"
  },
  "database": { "status": "ok", "pending_writes": 0, "dropped_writes": 0, "schema_version": 1 },
  "estimates": { "loaded": true, "estimated_at": "2026-10-05T10:01:00Z" },
  "accounts": [
    {
      "auth_index": "acct-1",
      "provider": "claude",
      "subscription": "claude",
      "tier": "max5x",
      "state": "estimated",
      "k": 6.0,
      "k_source": "auto",
      "k_auto": 6.0,
      "segment_window": "5h",
      "totals_from": "estimated",
      "safety_factor": 0.9,
      "observed_at": "2026-10-05T00:39:00Z",
      "five_hour": {
        "used_percent": 39,
        "reset_at": "2026-10-05T13:00:00Z",
        "total_usd": 8.5,
        "remaining_usd": 4.6665,
        "forecast": { "source": "reading", "per_hour_percent": 0, "outcome": "idle", "exhausts_at": null, "lookback_seconds": 3600 }
      },
      "week": {
        "used_percent": 39.83,
        "reset_at": "2026-10-10T04:00:00Z",
        "total_usd": 51.0,
        "remaining_usd": 27.6165,
        "forecast": { "source": "reading", "per_hour_percent": 1.11, "outcome": "eta", "exhausts_at": "2026-10-07T16:10:00Z", "lookback_seconds": 86400 }
      },
      "estimate": {
        "ratio": 11.76,
        "usd_per_point": 0.085,
        "clean_blocks": 6,
        "total_blocks": 6,
        "cold_start_blocks": 3,
        "segments": 228,
        "excluded_segments": 0,
        "voided_segments": 0,
        "basis": "raw",
        "estimated_at": "2026-10-05T10:01:00Z"
      },
      "model_weights": { "enabled": false, "basis": "raw", "suggestions": [] }
    }
  ]
}
```

### 管理接口

都需要 `Authorization: Bearer <admin_token>`，`admin_token` 为空时禁用。受 CPA 资源路由限制均为 GET。

| 路径（`/v0/resource/plugins/cpa-quota/` 下） | 作用 |
| --- | --- |
| `admin/rebuild` | 清空本实例的段、k 证据和处理位置，从 `requests` / `quota_report_windows` 重建；一次最多处理 `rebuild_budget_seconds`，余下的由后续刷新继续 |
| `admin/refresh-prices` | 立即拉取倍率表，失败返回 502 |
| `admin/calibrations/start?item=<id>&auth_index=<账号>` | 开始校准一个模型倍率条目 |
| `admin/calibrations/stop?id=<id>` | 结束校准 |
| `admin/calibrations/delete?id=<id>` | 删除校准 |

## safeguard 事件

Claude 以 `stop_reason: refusal`（或 `stop_details.type: refusal`、`category` 为 `cyber` 等）结束响应时，CPA 把请求变成 400 错误返回客户端，错误文本由 `clienterror.ClaudePolicyRefusalMessage` 生成，形如 `claude refusal (cyber): 原因`；这条请求同时以 `Failed=true`、`Failure.StatusCode=400`、`Failure.Body` 为该文本进入 `usage.handle`，流式与非流式相同。插件识别这些文本（以及 CPA 同样视为拒绝的 stop_details JSON 和 Anthropic 网络安全策略纯文本），写入 `safeguard_events`：

| 列 | 说明 |
| --- | --- |
| `instance`, `at_ns` / `at` | 实例、请求时间 |
| `auth_index`, `provider`, `model` | 账号、`claude`、计费模型名 |
| `category`, `reason` | 拒绝类别（如 `cyber`，可为空）、原因 |
| `request_id`, `status_code`, `stream` | CPA RequestID（去重键）、状态码、是否流式 |

插件不修改任何响应，也不做回调。

## 开发

```bash
go vet ./...
CPA_QUOTA_TEST_DSN='postgres://postgres:pw@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

需要数据库的测试读取 `CPA_QUOTA_TEST_DSN`，每个测试使用独立 schema 并在结束时删除；未设置时这些测试跳过。CI 用 PostgreSQL 16 service container。

端到端测试在真实的 CPA v8 中运行插件，配合 `scripts/dummy_upstream.py`（报告额度响应头、可触发拒绝的 Claude 上游）：

```bash
CPA_QUOTA_E2E_DSN='postgres://...' scripts/e2e_cpa_quota.sh 8.0.79   # 版本号或 cli-proxy-api 路径
```

不设置 `CPA_QUOTA_E2E_DSN` 时脚本用 Docker 启动 `postgres:16`。

## 发版

- `Changelog.md` 顶部的 `## vX.Y.Z` 与 `internal/plugin/types.go` 中的 `Version` 一致时，main 上 Check 通过后 Auto release 打 tag 并触发 Release。
- Release 构建 linux（amd64、arm64，manylinux2014）、darwin（amd64、arm64）、windows（amd64）产物 `cpa-quota_<版本>_<os>_<arch>.zip`（及 Unix 的 `.tar.gz`），生成 `checksums.txt`，发布说明取自 `Changelog.md`。
- `registry.json` 在仓库根目录，合入 main 后可供 CPA `plugins.store-sources` 使用。

## 许可

MIT，见 [LICENSE](LICENSE)。`internal/ccbilling` 的估算代码来自 sketchain/cpa-super-billing 中由该 fork 自行编写的文件；其余代码为本仓库按功能规格编写。
