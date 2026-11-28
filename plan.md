# plan.md — Issue #2：基于 local api key 的流量控制后台

状态：**approved**（2026-11-17 决策节点 1 获用户批准）

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

## 「完成」的度量判据（外部视角优先）

**外部可感知判据（真机）**

1. **M2 末真机迁移**：`~/.claude/settings.json` 中不再出现 GLM key 明文
   （`grep` 对比前后 + .bak diff 记入 run-log）；迁移后的 Claude Code 经代理完成
   一次真实会话；浏览器打开控制面首页能看到该 provider、key 与模型的真实数据
   （截图进 `docs/tasks/`，同时复制到 worktree 根 `tmp-m2-screenshots/`）。
2. **M3 末真实流量证据**：前端 token 统计显示的是迁移后真实使用的累计值
   （非 mock），与 `agentvault usage` 查询一致；配额真实生效一次（可观测的 429）。
3. **M4 末清零复核**：扫描器真机运行输出本地明文 key 落盘点为 0（run-log 附
   扫描输出），`.zshrc`/litellm/codex 全部迁移完成。

**内部工程判据**

4. `go test ./...` 全绿，含密钥泄漏扫描测试（断言仓库、日志、SQLite 内容、代理响应中不出现明文 key）。
5. 端到端冒烟：CLI 录入 mock provider（本地 httptest 上游）→ 代理启动 → curl 发起 chat completion → 返回 200 且响应为 OpenAI 格式。
6. 多 key 路由用例：上游对 key A 返回 429 → 代理自动切 key B 成功，且 A 进入冷却不再被选。
7. 用量用例：调用后 `agentvault usage` 能查到 provider/model/tokens 记录；quota 超限请求被 429 拒绝。
8. `go vet ./...`、`gofmt -l .` 干净。

## 非目标（本轮不做）

- 不做多用户/网络多租户：代理只监听 127.0.0.1，单机单用户。
- 不做计费/成本换算报表（只记 token 数）。
- 不引入 LiteLLM/Python 运行时依赖。
- 不做云端同步、团队共享 key。
- 不追求覆盖全部 provider 私有 API 格式，仅 OpenAI 兼容格式（+ 可配置 base_url 的 OpenAI 兼容上游）。

## 「上线」与反馈数据时间线（2026-11-17 评审：验收必须外部可感知）

本项目首要目标是**清理本地明文 key**，所以「上线」= 本机真实使用（dogfood）：

- **M2 末 = 第一次上线**：真机替换第一处明文（`~/.claude/settings.json` 的 GLM
  key → 代理），Claude Code 经代理实际工作；代理从此常驻，用量库开始积累真实
  feedback 数据。控制面前端 v0 同步上线，展示真实数据。
- **M3 末**：配额/熔断在真实流量下验证；前端 token 统计展示真机数据。
- **M4 末 = 最终验收**：`~/.zshrc`、litellm config、codex MCP 全部迁移，扫描器
  复核本地明文 key 清零 —— 项目首要目标的可感知完成。

## 控制面前端（v1 必含）

Go 二进制内嵌静态页（`net/http` + embed，无重型前端框架），本地
`http://127.0.0.1:<port>/` 展示三级视图（真机数据，禁止 mock 数据演示）：

1. 有哪些 provider；
2. 每个 provider 的每把 key 挂了哪些模型；
3. 每个模型累计消耗的 token 数。

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

### M2 — 本地代理、多 key 路由与第一次真机上线（约 7 任务）
OpenAI 兼容 HTTP 代理服务（127.0.0.1）、进程内取 key 注入上游请求、
轮询/加权路由策略、429/5xx 故障切换与 key 冷却、streaming (SSE) 透传、
**控制面前端 v0**（provider → key → 模型三级视图，内嵌静态页，此时 token 列
可为 0）、最小迁移路径（手工方式：把 `~/.claude/settings.json` 的 GLM key 录入
钥匙链并指向代理）。
**验收证据（外部可感知）**：mock 上游集成测试矩阵全绿；**真机替换第一处明文**
——settings.json grep 前后对比 + Claude Code 经代理真实会话 + 前端展示真实
provider/key/模型（截图），全部记入 run-log。

### M3 — 用量统计、流量控制与前端数据闭环（约 5 任务）
调用级用量记录（provider/model/tokens/latency/status）、`agentvault usage` 查询、
per-provider / per-key 配额（窗口期 + 超限 429）、熔断统计与冷却观测、
前端 token 统计接入真实用量库（三级视图第 3 层生效）。
**验收证据**：配额/用量测试全绿；前端展示迁移后真实流量累计 token（截图），
与 usage CLI 一致。

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

## M6 — 网关 auto 模式（owner 批准 2026-11-25, item 21；设计依据 docs/design/auto-mode.md §12）

块划分（每块完成后过里程碑决策节点 2，等人验收）：

- **M6-A** = M6-1（plans 表 + 价格/系数人工表 + verified_at）、M6-2（internal/auto
  决策层骨架：档位规则 v1 + 黑名单 + decision 记录）、M6-3（配额感知：GLM 隐藏
  接口客户端 + json 兜底 + 推断）。本块先做 M6-1/M6-2。
- **M6-B** = M6-4（缓存亲和：会话头识别 + 绑定表 + 代价函数）、M6-5（router 改造：
  设计稿 §10 五项）。
- **M6-C** = M6-6（owner 决策通道：pending/approve/deny，溢出唯一入口）、M6-7
  （错峰窗口配置 + 闲时放宽）、M6-8（观测面：usage 字段/响应头//admin 面板）。
- **M6-9** = 两周真实流量校准期 → 调参复盘（run-log 记录数据）。

实施纪律：devflow 三件套照旧；每步 lint+tests 绿；:8788 dev 端口先验；显式别名
直通行为回归不变；部署上线前过决策节点。

### M6-B 实施注记（2026-11-27）

- **M6-4 缓存亲和**：会话头优先级 x-claude-code-session-id > x-parent-session-id
  （父优先于子）> x-session-id > session-id，值须 ≥8 位 `[\w-]` 否则跳过该级；
  session → (provider, key, model) 绑定表纯进程内存（永不持久化），TTL 默认 2h
  （serve `--affinity-ttl` 可配，daemon 透传）；绑定失效条件 = TTL 过期 或 目标
  不健康（key 冷却/熔断或不再服务该模型）；粘性在 Decide 尾部生效（绑定且健康
  → 目标强制回绑定模型，reason 标 "sticky session kept"），重绑代价 = 估算丢失
  前缀缓存 tokens（ContextChars/4），记 usage `cache_rebind_cost` 并标
  matched_rule `/rebind`；能换 key 解决的不换 provider 由 router.acquirePreferred
  （绑定 provider 优先、其余 provider 兜底）保证。
- **M6-5 router 五项**：① cooldownState(model) 锁内只读快照（allCooling/soonest/
  breaker）；② 别名改写沉到 pick 之后逐 candidate 执行（candidate 携带该
  provider 自己注册的 upstream id，废除前置 first-match 改写，回归测试先行）；③
  auto 重判路径：目标别名全冷却 → ReDecide（deep/bg 永不重判；routine 在
  Routine ↔ LongContextTarget 间换目标，跳过不可用/当前）→ 改写 body → 重
  acquire，known() 的 404-vs-429 语义保持；④ gate 账户级并发闸门 auto 不旁路
  （acquirePreferred 复用同一 gate，集成测试验证 auto 排队）；⑤ M6-A 已达成
  （复核项）。
