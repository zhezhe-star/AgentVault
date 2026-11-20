# AgentVault 架构

M3-5 产出。对应 issue #2（local api key 流量控制后台）的 M1–M4 实现形态。

## 总览

单二进制、零外部服务的本地工具：

```
AI 客户端 (Claude Code / OpenAI 协议 CLI)
   │  占位符凭据 + base_url=http://127.0.0.1:8787
   ▼
┌────────────────────── agentvault serve ──────────────────────┐
│  proxy.Server                                                │
│   ├─ /v1/chat/completions   wireOpenAI   (Authorization)     │
│   ├─ /v1/messages           wireAnthropic (x-api-key)        │
│   ├─ /v1/models, /healthz, /admin/routing                    │
│   ├─ /                → console.Handler (embed UI)           │
│   ├─ router: 轮询 + 冷却 + 熔断(3 连败→5min→半开探测)          │
│   ├─ quotaChecker: 滚动窗口读 usage_records                   │
│   └─ keyCache: 钥匙链解密结果 TTL 5min 内存缓存               │
└──────┬────────────────────────────────────┬──────────────────┘
       │                                    │
       ▼                                    ▼
  provider.Store                        usage.Store
  (SQLite: providers/keys/              (SQLite: usage_records,
   models/quotas，key 仅存               WAL，writeMu 串行写)
   secret_id 引用)                          │
       │                                    │
       ▼                                 控制台/CLI 读取
  secret.Store (OS 钥匙链)  ◄── 代理进程内存中才有明文 key
```

## 密钥安全模型（核心约束）

**明文 key 只允许存在于两处：OS 钥匙链、代理进程内存。**

1. `secret.KeyringStore`（zalando/go-keyring）：key 明文存 macOS Keychain 等系统后端，service="agentvault"，account=`<provider>/<key-id>`。
2. `crypto.Envelope`：数据加密密钥（DEK）同样存钥匙链；AES-256-GCM 加密任意敏感载荷，base64(nonce|ciphertext)。
3. SQLite `keys` 表只有 `secret_id` 列——数据库整体被拖走也不含凭据。
4. 代理内存缓存（keyCache，TTL 5min + 故障即时 invalidate）是明文短暂停留的唯一位置；日志 RequestLog 在构造上只有 method/path/status/model。
5. `leakscan` 测试：合成 key 走完整管线后扫描 DB 字节（含 WAL）、源码树、错误信息与输出，断言零泄漏——把安全约束固化为回归测试。

## 代理转发核心

`forward(w, r, wireFlavor)` 是协议无关的单循环：

- **wire 抽象**：`wire{upstreamPath, setAuth}` 两字段描述一个协议。OpenAI = `/chat/completions` + `Authorization: Bearer`；Anthropic = `/v1/messages` + `x-api-key`。body 原样透传（不转换协议），仅白名单转发 `anthropic-version`/`anthropic-beta`/`accept`，客户端凭据头一律不转发。
- **候选循环**：router.pick 按 weight 轮询出候选 → 配额检查（超限 429，错误体含 `agentvault_quota_exceeded`/scope/ref_id，且该 429 也记 usage）→ 取 key → 建上游请求 → 发送。上游 429/5xx/网络错误：该 key 冷却并 failover 到下一候选；全部耗尽时中继最后一个上游状态与 body（保留 429 语义便于客户端重试），并补记一条 usage。
- **流式友好**：响应头复制后立即 WriteHeader，`flushCopy` 逐块写+Flush（SSE 无代理缓冲）；同时 TeeReader 进内存 buffer，流结束后 `usage.ExtractTokens` 从完整响应中抽取 token（JSON 对象或 SSE `data:` 行，OpenAI `prompt/completion_tokens` 与 Anthropic 嵌套 `message.usage` 的 `input/output_tokens` 双形态，最后一个非零值获胜）。

## 路由容错

- **冷却**：失败 key 60s（DefaultCooldown）内不被选中；成功即清零。
- **熔断**：连续失败达 3（BreakerThreshold）升级为 5min 熔断并打 breaker 标记；到期后该 key 被选中即为半开探测——成功闭合、失败立即重开。
- **观测**：`/admin/routing` 输出 cooling_keys/breaker_keys/provider 计数，`agentvault status` 与控制台消费。

## 配额

`quotas` 表：(scope, ref_id) → (window_seconds, max_tokens)，scope 取 provider 或 key。检查在请求前做：对 usage_records 按 ref 求滚动窗口内的 in+out 总和，≥ max 即拒。语义是「超限后的下一请求被拒」（调用前无法预知本次 token 数）；历史持久在 DB，跨重启有效。

## 数据层

- 单个 SQLite 文件（`~/.config/agentvault/agentvault.db`）承载 provider 与 usage 两个 Store（各自连接，WAL 允许并发读写）。
- 前向迁移：`schema_migrations` 记录版本，migrations 切片顺序执行，每条恰好一次。
- usage 写入用互斥锁串行化——busy_timeout 是每连接语义，50 并发写测试曾暴露 SQLITE_BUSY，锁是正确解。
- 时间统一 UTC RFC3339Nano 字符串存储与比较。

## 控制台

embed 静态页挂代理根路径（内部 mux 精确匹配 `/console/api/overview`）；三级视图 provider → key(ref) → 模型，模型层 token 数来自 usage 实时聚合。仅回环可达；overview 不含任何凭据字段（key 只显示 ref 引用）。handler 以闭包每请求重建，避免构造顺序导致的空 usageStore。

## M4（规划中）：明文清零迁移

扫描本地各点文件（~/.zshrc、~/.claude/settings.json、~/.config/litellm/config.yaml、~/.codex/config.toml）中的明文 key，按消费方式三分类：proxy-attach（改 base_url 走代理）/ run-inject（`agentvault run` 解密注入子进程 env）/ compliant（不动）。import 预览确认 + .bak 备份；改写真实点文件前需逐项人工批准。
