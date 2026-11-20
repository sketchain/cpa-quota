# Changelog

## v0.1.0

### 升级须知

- 首个版本。需要 CLIProxyAPI v8 和 PostgreSQL；在 `plugins.configs.cpa-quota` 中必须配置 `instance` 和 `postgres_dsn`。

### 后端

- 按订阅额度估算 Claude（Pro、Max 5x、Max 20x）和 ChatGPT Team 账号的 5 小时与 7 天总额和剩余额度（USD）：按利用率 1% 切段，滑动窗口过滤被外部用量污染的段，由两窗口同期增量估算 k，干净大块不足时冷启动使用档位默认周总额，剩余额度乘安全系数。
- 主窗口内费用没能算出来的请求（模型未定价，或用量不完整、不可计费）使其所在的段作废，不参与总额估算和模型倍率估算 / 校准，所有订阅相同；作废段按原因（`unpriced`）计数，额度接口给出 `estimate.voided_segments`，诊断中列出 Claude 和 ChatGPT 的未定价模型。ChatGPT 另一个额度池的请求只剔除自身费用，段不作废。
- 从 new-api 格式的倍率表换算模型价格，每 24 小时刷新，失败时使用上次保存的倍率表。
- 所有实例共用一个 PostgreSQL，自动建表和迁移；数据库不可用时写入暂存在内存并在恢复后按顺序补写。
- 提供只读额度接口 `/v0/resource/plugins/cpa-quota/quota`，以及从历史重建、刷新倍率表和模型倍率校准的管理接口。
- 记录 Claude 安全策略拒绝（safeguard）事件。
