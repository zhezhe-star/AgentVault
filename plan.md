# plan.md — Issue #2：基于 local api key 的流量控制后台

状态：**draft**（待决策节点 1 批准）

## 背景

AgentVault 定位为本地加密 API key 管理项目（起因：OpenCode 密钥泄露事件，见
`docs/PROJECT_BACKGROUND.md`）。Issue #2 要求在密钥管理之上分出「本地路由管理 +
流量管理」模块：本地代理进程对外提供统一 OpenAI 兼容入口，对内管理多个 Provider
的真实 API key（明文永不落盘/不进代码）。

## 技术栈决策

- **语言：Go**（用户指定倾向）。
- 参考/借鉴项目（M1 任务 A1 中深入调研并落文档）：
  - [QuantumNous/new-api](https://github.com/QuantumNous/new-api)（Go，one-api 系，多 key 路由 + 配额 + 用量）
  - [songquanpeng/one-api](https://github.com/songquanpeng/one-api)（new-api 上游，渠道/令牌模型参考）
  - [bricks-llm](https://github.com/bricks-llm/ai-gateway)（Go，API key 管理网关参考）
  - [LiteLLM](https://github.com/BerriAI/litellm)（Python；不引入运行时依赖，仅参考其路由/降级语义）
  - 多 key 轮换 + cooldown 模式（Reddit 上开源 Go 插件实践，见 run-log 引用）
- OS 钥匙链：Go 侧用 `zalando/go-keyring`（macOS Keychain / libsecret / Windows Credential Manager）。
- 元数据与用量存储：SQLite（只存密文引用与用量数字，绝不存明文 key）。

## 目标状态

完成后：用户通过 `agentvault` CLI 录入一次 Provider 及其 key（进 OS 钥匙链），
启动本地代理后，任意 OpenAI 兼容客户端把 base_url 指向 `http://127.0.0.1:<port>`
即可用请求；代理按策略在多把 key/多个 provider 间路由、故障切换与冷却，并把
每次调用的 token 用量记入本地库供查询与限额。

## 「完成」的度量判据

1. `go test ./...` 全绿，含密钥泄漏扫描测试（断言仓库、日志、SQLite 内容、代理响应中不出现明文 key）。
2. 端到端冒烟：CLI 录入 mock provider（本地 httptest 上游）→ 代理启动 → curl 发起 chat completion → 返回 200 且响应为 OpenAI 格式。
3. 多 key 路由用例：上游对 key A 返回 429 → 代理自动切 key B 成功，且 A 进入冷却不再被选。
4. 用量用例：上述调用后，`agentvault usage` 能查到本次调用的 provider/model/tokens 记录；为 provider 设 quota 后超限请求被 429 拒绝。
5. `go vet ./...`、`gofmt -l .` 干净。

## 非目标（本轮不做）

- 不做 Web 图形控制台（后续 issue）。
- 不做多用户/网络多租户：代理只监听 127.0.0.1，单机单用户。
- 不做计费/成本换算报表（只记 token 数）。
- 不引入 LiteLLM/Python 运行时依赖。
- 不做云端同步、团队共享 key。
- 不追求覆盖全部 provider 私有 API 格式，仅 OpenAI 兼容格式（+ 可配置 base_url 的 OpenAI 兼容上游）。

## 明文迁移策略（按消费方式三分类，2026-11-17 基于真机扫描确认）

本地已确认的明文落盘点：`~/.zshrc`（ARK_API_KEY export）、`~/.claude/settings.json`
（ANTHROPIC_AUTH_TOKEN + BASE_URL）、`~/.config/litellm/config.yaml`（model_list 明文
api_key）、`~/.codex/config.toml`（MCP server env 块：ark / Z_AI key）。

| 分类 | 判据 | 处理方式 |
|---|---|---|
| **proxy-attach** | LLM chat 流量、客户端支持改 base_url（claude settings、zshrc ANTHROPIC_*、litellm config） | key 入钥匙链，base_url 改指向 `agentvault serve`，明文从配置删除 |
| **run-inject** | 非 LLM 的工具/MCP 服务凭据（codex MCP env 块等），消费方直连自己的 API，代理无法居中 | `agentvault run --env VAR=ref:... -- <cmd>` 启动时解密注入环境变量；一工具一专用 key，爆雷单独吊销 |
| **compliant** | 走 os.environ/secret manager 引用，无明文落盘 | 不处理，报告中标注即可 |

扫描器输出必须带分类标签；import 改写只动前两类，且必须先预览确认 + .bak 备份。
改写用户点文件（.zshrc、settings.json 等）前必须单独获得批准（决策节点 3）。

## 约束与不变式

- **明文 key 只存在于 OS 钥匙链与进程内存**；不进源码、配置文件、日志、SQLite、错误信息、测试 fixture（测试用假 key 格式即可）。
- 仓库开源友好：无硬编码凭据、无个人路径依赖、CI 可跑。
- 代理默认仅绑定 `127.0.0.1`，不做鉴权旁路；如后续加 token 鉴权另立任务。
- SQLite schema 变更走迁移文件，不直接 ALTER 破坏兼容。

## 失败模式与回滚

| 失败模式 | 缓解/回滚 |
|---|---|
| keyring 在 CI/Linux 无桌面环境不可用 | 抽象 `SecretStore` 接口；CI 用可注入的内存/file 后端（file 后端仅限测试，文档明确标注不安全） |
| 上游全部 key 失效/冷却 | 代理返回明确 503 + 错误体，客户端可感知；不 crash |
| SQLite 用量写坏/锁死 | WAL 模式 + 每里程碑备份 `*.db`；schema 迁移可回退 |
| 代理 panic/泄漏内存 | 每请求 recover + 结构化日志；里程碑全量测试 |
| 加密实现自造轮子出错 | 只用标准库/成熟库（AES-GCM via stdlib），密钥派生逻辑保持最小并加测试 |

## 里程碑划分

### M1 — Provider 核心与加密存储（约 6 任务）
Provider 实体模型、SQLite schema（迁移机制）、`SecretStore` 抽象 + go-keyring 实现、
AES-GCM 数据加密层、控制面 CLI（provider add/list/show/revoke）、密钥泄漏扫描测试。
**验收证据**：`go test ./...` 含泄漏扫描全绿；CLI 端到端录入/吊销演示输出。

### M2 — 本地代理与多 key 路由（约 5 任务）
OpenAI 兼容 HTTP 代理服务（127.0.0.1）、进程内取 key 注入上游请求、
轮询/加权路由策略、429/5xx 故障切换与 key 冷却、streaming (SSE) 透传。
**验收证据**：httptest mock 上游的集成测试矩阵全绿；手动 curl 冒烟记录。

### M3 — 用量统计与流量控制（约 5 任务）
调用级用量记录（provider/model/tokens/latency/status）、`agentvault usage` 查询、
per-provider / per-key 配额（窗口期 + 超限 429）、熔断统计与冷却观测、
README/设计文档（含借鉴项目调研结论 docs/design/survey.md）。
**验收证据**：配额超限测试、用量查询测试全绿；文档合入。

### M4 — 明文迁移与接入替换（约 5 任务）
按消费方式三分类的明文扫描器（proxy-attach / run-inject / compliant）、
import 导入（预览→确认→改写+.bak）、proxy-attach 接入改写（base_url 指向代理、
shell rc 注释化）、`agentvault run` 环境变量注入（MCP/工具凭据场景）、
迁移报告。**验收证据**：fixture HOME 下的扫描/导入/改写集成测试全绿；
真机试运行记录进 run-log（改点文件前经决策节点 3 批准）。

## 熔断参数

同一修复连续失败 **N=3** 次即停，写 failure 诊断条目等人指示。

## 技术方案要点

- 目录：`cmd/agentvault`（CLI+代理入口）、`internal/provider`（实体+存储）、
  `internal/secret`（SecretStore）、`internal/crypto`（AES-GCM 信封加密）、
  `internal/proxy`（HTTP 代理+路由）、`internal/usage`（用量+配额）。
- key 组织：`provider → keys[]`，每把 key 独立 secret 条目（keychain service
  `agentvault/<provider-id>`，account = key-id），DB 只存 key-id、状态、权重、冷却元数据。
- 信封加密：数据密钥（DEK）存钥匙链，AES-GCM 加密 DB 内可能残留的敏感字段（如 base_url 中的凭据参数）；key 本体不进 DB。
- 路由：默认 round-robin + cooldown（429 → 冷却 60s 可配）；每 key 独立熔断计数。
- CLI 框架：`spf13/cobra`；配置：`~/.config/agentvault/config.toml`（不含任何密钥）。
