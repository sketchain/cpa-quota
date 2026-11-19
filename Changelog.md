# Changelog

## v0.1.0

### 升级须知

- 首个版本。需要 CLIProxyAPI v8 和 PostgreSQL；在 `plugins.configs.cpa-quota` 中必须配置 `instance` 和 `postgres_dsn`。

### 后端

- 按订阅额度估算 Claude（Pro、Max 5x、Max 20x）和 ChatGPT Team 账号的 5 小时与 7 天总额和剩余额度（USD），沿用 cpa-super-billing 的 1% 切段、污染过滤、k 估算、冷启动和安全系数算法。
- 从 new-api 格式的倍率表换算模型价格，每 24 小时刷新，失败时使用上次保存的倍率表。
- 所有实例共用一个 PostgreSQL，自动建表和迁移；数据库不可用时写入暂存在内存并在恢复后按顺序补写。
- 提供只读额度接口 `/v0/resource/plugins/cpa-quota/quota`，以及从历史重建、刷新倍率表和模型倍率校准的管理接口。
- 记录 Claude 安全策略拒绝（safeguard）事件。
