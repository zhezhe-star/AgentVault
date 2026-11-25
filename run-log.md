# run-log.md — 证据日志

条目格式：
```
## [ISO 时间] 类型: 一句话标题
- 细节 / 证据 / 命令输出摘录
```
类型：decision / evidence / failure / milestone / checkpoint / quota-stop / evolve / release / publish

---

## [2026-11-17T00:00:00+08:00] decision: kickoff — Issue #2 需求澄清结论（devflow v1.2.0）
- 工具链版本：`~/.orca/devflow/VERSION` = 1.2.0 (commit b02f951)。本仓非 dev-flow toolkit 仓，跳过 best-practices 同步检查。
- nightLM 实为基于 LiteLLM 的本地代理；用户澄清后技术栈定为 **Go**（不引入 LiteLLM 运行时依赖，仅借鉴路由语义）。
- 本轮范围 = 代理 + 多 key 路由 + 用量统计（不含 Web 控制台，列非目标）。
- `zhezhe-star/issue-1` 分支与 main 相同（无架构产出），故 M1 先落 issue #1 的最小核心（Provider 实体 + 加密存储）。
- 明文不落盘路线：OS 钥匙链为主（zalando/go-keyring），测试环境用可注入内存后端。
- 调研候选：QuantumNous/new-api（Go）、songquanpeng/one-api、bricks-llm、LiteLLM（语义参考）、Go multi-key rotation+cooldown 插件实践。深入调研放在 M3-5 的 docs/design/survey.md。
- 熔断参数 N=3（默认）。

## [2026-11-17T00:10:00+08:00] decision: 新增 M4 明文迁移与接入替换（三分类策略）
- 真机扫描确认四类明文落盘点：~/.zshrc（ARK_API_KEY）、~/.claude/settings.json（ANTHROPIC_AUTH_TOKEN）、~/.config/litellm/config.yaml（model_list api_key）、~/.codex/config.toml（MCP env 块：ark / Z_AI key）。
- 策略按消费方式三分类：proxy-attach（改 base_url 走代理）/ run-inject（agentvault run 解密注入子进程 env，一工具一专用 key）/ compliant（不动）。
- import 必须预览确认 + .bak 备份；改写用户点文件前需单独批准（决策节点 3）。
- checklist 由 16 任务扩至 21 任务（M4 共 5 条）；checklist 中 M3-5 现列于 M4 之后，阅读顺序不影响语义。

## [2026-11-17T00:20:00+08:00] decision: 里程碑重构 — 外部可感知验收 + 控制面前端提前进 v1
- 用户评审意见：验收证据必须外部可感知；项目首要目标是清理本地明文 key，不能「始终不上线」。
- 重构：M2 末 = 第一次真机上线（替换 ~/.claude/settings.json GLM key → 代理，Claude Code 真实会话）；控制面前端 v0 提前进 v1（provider → key → 模型三级视图，内嵌静态页，禁止 mock 数据）。
- M3 末：前端 token 统计接入真实用量库，真实流量下验证配额 429。
- M4 末 = 最终验收：真机扫描明文落盘点清零。
- 新增外部判据 3 条（真机迁移证据 / 真实流量证据 / 清零复核），置于「完成」度量判据首位。
- checklist 21 → 24 任务（新增 M2-6、M2-7、M3-6）。

## [2026-11-17T00:30:00+08:00] decision: 决策节点 1 通过 — 计划批准，进入执行循环
- 用户明确批准三件套（24 任务，M1-M4）。plan.md 状态改 approved。
- Go 工具链：go1.26.6 darwin/arm64。开始 M1-1（仓库脚手架）。

## [2026-11-18T00:05:00+08:00] evidence: M1-1 仓库脚手架完成
- go mod init github.com/zhezhe-star/AgentVault；cobra v1.10.2。
- 目录：cmd/agentvault + internal/{provider,secret,crypto,proxy,usage,console}。
- `go build ./...` ✅ `go vet ./...` ✅ `gofmt -l .` 空 ✅ `agentvault --help`/`version` 正常。
- CI workflow（gofmt/vet/test）+ Makefile + .gitignore（bin/ *.db .bak tmp-screenshots）。

## [2026-11-18T00:15:00+08:00] evidence: M1-2 Provider 实体与 SQLite schema 完成
- modernc.org/sqlite（纯 Go，无 cgo，开源 CI 友好），WAL + busy_timeout=5000。
- 表：providers / keys（status: active|disabled|revoked）/ models（UNIQUE(key_id,name)）+ schema_migrations 前向迁移，Open 可重复执行幂等。
- 密钥隔离：keys 表只存 secret_id 引用，无明文列。
- 测试：迁移幂等、provider/key/model 生命周期、状态流转、重复模型 UNIQUE 拒绝 —— `go test ./internal/provider/` ok，vet/gofmt 干净。

## [2026-11-18T00:25:00+08:00] evidence: M1-3 SecretStore 抽象完成
- Store 接口（Set/Get/Delete）+ KeyringStore（zalando/go-keyring v0.2.8，生产后端）+ MemoryStore（测试/CI）。
- 服务名命名空间 "agentvault"，按 service/account 隔离；go-keyring.ErrNotFound 映射为包内 ErrNotFound。
- 测试：往返、账号/服务隔离、删除后 NotFound —— go test ./internal/secret ok；vet/gofmt 干净。
- MemoryStore 文档明确标注不得用于生产。

## [2026-11-18T00:35:00+08:00] evidence: M1-4 AES-GCM 信封加密层完成
- Envelope：DEK（32B）首次使用时生成并存钥匙链（account=data-encryption-key），base64(nonce|ciphertext) AES-256-GCM。
- 测试 5 项全过：加解密往返、篡改密文报 ErrBadCiphertext、错误 DEK 失败、同明文密文随机化（nonce 唯一）、跨 Envelope 复用同一 DEK。vet/gofmt 干净。

## [2026-11-18T02:30:00+08:00] evidence: M1-5 控制面 CLI 完成
- provider add/list/show/revoke + add-key（TTY 隐藏回显；管道场景限读 64KB）。
- 沙箱演示（AGENTVAULT_HOME=mktemp + memory 后端）：add → add-key → list/show 显示 ref:bigmodel/primary → revoke 后 status=revoked；输出无明文。
- `grep fake-glm-key agentvault.db` 无命中（exit 1）。CLI 集成测试断言 stdout/secret 隔离/撤销状态全过；全仓测试 + vet + gofmt 绿。

## [2026-11-18T02:45:00+08:00] evidence: M1-6 密钥泄漏扫描测试完成 — M1 里程碑达成
- leakscan 包：随机合成 key 注入完整管线（provider store + memory SecretStore + envelope），断言 SQLite 字节（含 -wal）、源码树、错误信息、输出均无明文。
- 全仓 `go test ./...` 5 包 ok（cmd/provider/secret/crypto/leakscan），vet/gofmt 干净。
- **M1 里程碑完成**：6/6 任务 complete。M1 验收口径（测试全绿 + CLI 演示）满足，证据见 02:30/02:45 条目。

## [2026-11-18T03:00:00+08:00] evidence: M2-1 代理 HTTP 服务完成
- Server：/v1/chat/completions（POST 转发 OpenAI 格式）、/v1/models（聚合 models 表）、/healthz。
- 仅绑 127.0.0.1（非回环地址直接拒绝，有测试）；panic recover 中间件返回 500 JSON；响应逐块 flush（SSE 友好）。
- 测试：转发正确（Authorization=Bearer 注入、路径 /chat/completions、body 透传、响应 OpenAI 格式）、loopback 拒绝、panic 500、models 列表、未知模型 404 且不泄漏 key。
- go test ./... 6 包 ok；vet/gofmt 干净。

## [2026-11-18T03:10:00+08:00] evidence: M2-2 key 注入 TTL 缓存完成
- keyCache：解密 key 进程内存缓存 TTL 5 分钟，invalidate 支持故障后立即刷新；miss 映射 ErrNotFound。
- 日志结构化排除：RequestLog 只含 method/path/status/model，header/body/key 构造上不可进入日志（无脱敏逻辑可出错）。
- 测试：TTL 内不读新值、invalidate 立即刷新、miss 分类、Flush 透传（SSE 不被日志层缓冲）。全仓 6 包 ok，vet/gofmt 干净。

## [2026-11-18T03:30:00+08:00] evidence: M2-3 多 key 路由完成（含 bug 修复一条）
- router：weight 降序 + keyID 稳定排序的候选列表、round-robin、429/5xx 失败即冷却 60s 并 failover、成功清零、coolingCount 观测。
- **修复 rr 初值 bug**：map 零值 0 导致首轮跳过第一个候选（debug 测试定位：candidates 顺序正确而 pick 落在 b2），改为缺省视为 -1。
- 测试：A 429 → 同请求切 B 成功且 A 冷却不再被选（后续 3 请求全走 B）、冷却到期恢复、全部 key 耗尽中继 429、错误体不泄漏。全仓 6 包 ok，vet/gofmt 干净。

## [2026-11-18T03:45:00+08:00] evidence: M2-4 SSE streaming 透传完成
- 测试：mock 上游逐块推送 SSE → 代理透传 3 个 data 块（含 [DONE]），Content-Type 保留 text/event-stream，首块无缓冲延迟（flushCopy 逐块 Flush）。
- 流中断用例：上游 mid-stream 断连（http.ErrAbortHandler）→ 客户端收到流关闭而非挂死（5s 超时护栏）。全仓 6 包 ok，vet/gofmt 干净。

## [2026-11-18T04:00:00+08:00] evidence: M2-5 代理生命周期 CLI 完成
- serve（--daemon setsid 后台 + pid 文件）、stop（SIGTERM 优雅退出）、status（pid 存活 + /admin/routing 冷却数 + provider/key/model 计数）。
- 新增 /admin/routing 端点（loopback-only listener 保护）。
- 真实沙箱演示：serve --daemon → status 显示 pid/冷却/计数 → curl /healthz ok → stop → status running=false。全仓 6 包测试 ok，vet/gofmt 干净。

## [2026-11-18T04:15:00+08:00] evidence: M2-6 控制面前端 v0 完成
- internal/console：embed 静态页 + /console/api/overview，三级视图 provider → key(ref) → 模型(token 列为真实 0，M3 接用量库)。
- 挂载到代理同进程：/console/ 与 / 静态服务；仅 127.0.0.1（复用 listener 保护）。
- 测试：overview 返回真实库数据、index 可访问、即使后端有 secret API 也不含密钥。全仓 7 包 ok，vet/gofmt 干净。

## [2026-11-20T19:30:00+08:00] decision: 决策节点 3 通过 — M2-7a + M2-7 真机迁移批准
- 用户询问协议层开源复用件，调研结论：Olla（Go，/v1/messages passthrough）、m0n0x41d/anthropic-proxy-rs（Rust，协议转换）、Tangerg/scope（Go 线协议库）、LiteLLM passthrough。均不引入——代理是透明透传（仅注入凭据头），自实现约 50 行，依赖面更小。
- 方案：M2-7a = /v1/messages 透传 + providers.anthropic_base_url 字段；M2-7 = ~/.claude/settings.json 真机迁移。用户「批准」。

## [2026-11-20T19:50:00+08:00] evidence: M2-7a /v1/messages 透传完成（commit d1e8d30）
- migration 4：providers.anthropic_base_url（默认空 = OpenAI-only）；provider add 增 --anthropic-base-url。
- forward 循环协议无关化：/v1/messages 透传 body 与 anthropic-version/anthropic-beta/accept 头，注入 x-api-key；客户端凭据头不转发。
- 新增 provider add-model 命令（M1 CLI 缺口补齐）。测试：透传/OpenAI-only 拒绝/故障切换 3 用例；全仓 8 包 ok，vet/gofmt 干净。

## [2026-11-20T19:55:00+08:00] evidence: M2-7 第一次真机迁移完成 — 首次上线
- 迁移对象：~/.claude/settings.json（GLM key，指纹 f693…xx5d18 掩码记法）。备份 settings.json.bak-20261120-193220。
- 步骤：provider add bigmodel（含 anthropic-base-url）→ add-key primary（token 从 settings.json 管道入 macOS 钥匙链，不回显）→ add-model ×2（glm-5.3-flash / glm-4.5-air）→ serve --daemon（pid 12098, 127.0.0.1:8787）。
- 冒烟：curl 经代理 /v1/messages 真实返回 bigmodel Anthropic 格式响应（真实 token usage: input 20/output 64）。
- 改写：ANTHROPIC_BASE_URL → http://127.0.0.1:8787；ANTHROPIC_AUTH_TOKEN → 占位符 agentvault-local-placeholder（真 key 仅存钥匙链）。diff 仅此两处，其余 env 原样。
- 真机验证：claude -p 无头会话经 127.0.0.1:8787 成功获得 glm-5.3-flash 真实回复（Claude Code 侧确认请求过代理网关）。
- 无明文复核：grep 真 key 于改写后 settings.json 零命中；strings agentvault.db 仅含元数据与 ref:bigmodel/primary；钥匙链 security find-generic-password 确认 bigmodel/primary 存在。
- 控制台：/console/api/overview 返回真实三级结构（bigmodel → primary → 2 模型，token 列真实 0 待 M3）；截图 docs/tasks/m2-7-console.png（tmp-m2-screenshots/ 同步）。
- **修复 M2-6 遗留 bug 一条**：console 以 /console/ 前缀挂载时 FileServer 在 embed FS 内查 "console/..." 导致 UI 404（API 正常故测试未拦住）；改为 Handler 挂根（内部 mux 精确匹配 /console/api/overview），补 TestConsoleMountedAtRoot 回归。截图为修复后真实页面。

## [2026-11-20T20:20:00+08:00] checkpoint: 决策节点 — M2 里程碑完成，用户确认进入 M3

## [2026-11-20T20:40:00+08:00] evidence: M3-1 调用级用量记录完成
- internal/usage：usage_records 表（同 DB 文件，独立建表 + 写锁串行化——busy_timeout 仅作用于单连接，50 并发写测试暴露 SQLITE_BUSY 后加 writeMu）。
- 代理 forward 循环：tee 响应（流式友好），ExtractTokens 双协议解析——OpenAI（prompt/completion_tokens，SSE 末 chunk）与 Anthropic（input/output_tokens，message_start/message_delta）；全 key 耗尽的中继失败也记一条（status=上游状态，tokens=0）。
- serve 接线：SetUsageSink → usage.Insert，记录失败不影响请求。
- 测试：落库往返、50 并发无锁死计数正确、OpenAI/Anthropic 各 JSON+SSE 四种形态、垃圾输入零值；proxy 集成测试一次调用恰一条记录且字段完整。全仓 9 包 ok。
- 真机验证：重启代理后真实 /v1/messages 调用落库 bigmodel|primary|glm-5.3-flash|anthropic|200|17|32|1197。

## [2026-11-20T20:55:00+08:00] evidence: M3-2 usage 查询 CLI 完成
- agentvault usage：--provider/--key/--model/--since/--until/--limit/--json；since/until 接受 RFC3339 或相对时长（1h/30m）。
- 修复两处：时间窗比较统一 UTC（created_at 为字符串比较，本地 +08:00 会排错序）；Record 加 snake_case json tags。
- 修复 proxy 集成测试竞态（sink 在响应 flush 后执行，改轮询等待，-count=5 稳定）。
- 测试：过滤/JSON/时间窗用例全过；真机 `usage --limit 5` 显示用户当前真实会话经代理的流量记录（bigmodel glm-5.3-flash 多条 200，tokens 实值）。全仓 9 包 ok。

## [2026-11-20T21:15:00+08:00] evidence: M3-3 配额控制完成
- quotas 表（migration 5，scope=provider|key + window_seconds + max_tokens）+ provider quota CLI（--scope/--window/--max-tokens，upsert）。
- 代理语义：请求前查滚动窗口实际用量（读 usage_records，跨重启有效），已达上限即 429（调用前无法预知本次 token，语义为「超限后的下一请求被拒」）；429 错误体含 agentvault_quota_exceeded/scope/ref_id；配额 429 也记 usage（可观测）。
- 测试：per-key 耗尽（2 过 3 拒，历史总和恰等于上限）、per-provider 独立生效、窗口滚动恢复（1s 窗实测）、错误体结构与无泄漏。-count=3 稳定。全仓 9 包 ok。
- 部署注意：live 代理启用配额检查但未对 bigmodel 设真实配额（空配置=不限制），避免打断用户当前会话；M3-6 验收需 429 时再临时设小配额。

## [2026-11-20T21:30:00+08:00] evidence: M3-4 熔断与恢复探测完成
- router 熔断：连续失败计数达 BreakerThreshold=3 → 熔断（冷却升级为 BreakerOpenDuration=5min，breaker 标记）；冷却到期后首次被选即半开探测——成功清零闭合，失败（计数仍≥阈值）立即重开。
- 观测：/admin/routing 增 breaker_keys，status CLI 输出 breaker keys 行。
- 测试：TripAndHalfOpenRecovers 全链路（触发→探测前拒绝→探测放行→失败重开→成功闭合→正常服务）-count=3 稳定。全仓 9 包 ok。
- 遗留说明：全 key 耗尽仍中继最后上游状态（429）——保留原语义（客户端可重试），未改 503；已在 M2-3 记录。

## [2026-11-20T22:00:00+08:00] evidence: M3-6 前端 token 统计接入真实用量库完成
- console Handler 增加 usage store 参数，模型层 SumTokens 聚合真实 tokens_in/out；代理经闭包每请求重建 handler（否则 New 时 usageStore 仍为 nil，真实页面 0/0——测试未拦住的第二个挂载类 bug）。
- 三方一致性核对：console / `agentvault usage` / 直接 SQL 三处 token 数字一致（当时 glm-5.3-flash 128073/56227）。
- 真实流量 429 验收：provider bigmodel 设 quota 3000 tokens/1m → 下一请求即 429，错误体 `{"error":{"message":"quota exceeded","ref_id":"bigmodel","scope":"provider","type":"agentvault_quota_exceeded"}}`；429 行落 usage_records（含用户 live 会话两条，客户端重试成功）；DELETE 配额后恢复 200。
- 截图 docs/tasks/m3-6-console-tokens.png（真实累计：glm-5.3-flash 144494/65129、glm-4.5-air 351/14577）。全仓 9 包 ok，vet/gofmt 干净。

## [2026-11-20T22:20:00+08:00] evidence: M3-5 文档收尾完成 — M3 里程碑达成
- README 增快速上手四步：安装（go build）/ 录入（add + 管道 add-key 不回显）/ serve --daemon / 客户端接入（OpenAI env 与 Claude Code settings.json 两形态，占位符凭据）+ usage/quota 用法 + 路由容错与安全模型说明。
- docs/design/survey.md：new-api / one-api / bricks-llm / LiteLLM 各自可借鉴点与不引入理由（透明透传无需协议转换层；Go 栈不引 Python 运行时；三者均要求上游 key 明文存其 DB，与本项目安全前提冲突）；末尾列明自实现边界与仅有的三个外部依赖（cobra/go-keyring/modernc sqlite）。
- docs/design/architecture.md：单二进制架构图、密钥安全模型（明文仅存钥匙链+进程内存，leakscan 固化为回归测试）、wire 抽象转发循环、路由容错/配额/数据层/控制台设计、M4 三分类迁移规划。
- 全量套件 -count=1 重跑绿（9 包 ok），vet/gofmt 干净。
- **M3 里程碑完成**：M3-1…M3-4、M3-6 全部 complete（M3-5 本条收尾）。M3 验收口径满足：真实流量 token 统计三方一致（M3-6 证据）+ 真实流量 429 可观测（M3-6 证据）+ 文档齐备（本条）。

## [2026-11-22T00:05:00+08:00] decision: 决策节点 — M3 里程碑确认，用户批准进入 M4
- 用户「继续，进入 M4」。M4 目标 = 最终验收：真机明文落盘点清零。改写真实点文件前逐项经决策节点 3 批准。

## [2026-11-22T00:10:00+08:00] evidence: M4-1 明文扫描器完成
- internal/migrate：四类解析器（shell export / JSON env 块 / YAML api_key / TOML env 块），覆盖 .zshrc/.zshenv/.bashrc/.claude/settings.json/litellm config.yaml/.codex config.toml；三分类（proxy-attach/run-inject/compliant，默认 claude+litellm 走代理、shell 与 MCP env 走 run-inject，SetClassification 可覆盖）。
- 输出构造上无完整明文：Mask 仅前 4+后 4+长度；占位符（agentvault-local-placeholder/sk-xxx）与变量引用（${...}）不报——后者是真机首跑发现的误报（.zshrc 第 9 行已是引用），补过滤并加回归用例。
- 测试：fixture HOME 全四文件、掩码不泄漏、占位符/非凭据变量不报、默认分类与覆盖、空 home、Mask 单测。全仓 10 包 ok。
- 真机只读扫描：11 处明文——.zshrc ARK_API_KEY（ark-…fc2a）、litellm config 3 处 api_key（其中 GLM 两处同值 f693…hXcM 即已迁钥匙链的 bigmodel key、另 deepseek sk-d…a22f）、.codex config.toml 7 处（ark ×3、GLM f693…hXcM ×3、Z_AI f693…hXcM 同值、deepseek sk-9…0119）；~/.claude/settings.json 零命中（M2-7 迁移保持有效）。

## [2026-11-22T00:40:00+08:00] evidence: M4-2 import 导入命令完成
- internal/migrate/import.go：BuildPlan（跳过 compliant，ref=provider/keyID）→ Preview（掩码，只读）→ Apply（两阶段：先全部入 SecretStore，后逐文件 .bak-时间戳 备份 + 改写——存储全成功才动文件，部分失败零丢失）。
- 改写策略：shell export 行明文彻底移除，替换为注释（含 ref 与 `agentvault run --env VAR=ref:... -- cmd` 恢复指引）；JSON/YAML/TOML 值替换为 agentvault-local-placeholder（保持结构合法）。
- **修复两处设计缺陷**（测试驱动）：同变量多文件引用冲突（.zshrc 与 codex 的 ARK_API_KEY 同名，后者覆盖前者）→ ref 按文件名去重（ark/ark_api_key vs ark/ark_api_key_config）；JSON env 块无行号锚点 → 按键名全文档精确替换而非行级改写。
- 测试：预览只读快照断言、Apply 后三方验证（每个 ref 明文与原文件一致 / 每文件恰一个 .bak 且含原文 / 改写后文件零明文）、shell 行含恢复指引、toml/yaml 占位符、ref 去重。-count=3 稳定；全仓 10 包 ok，vet/gofmt 干净。
- CLI：`agentvault import` 默认预览，--yes 才应用（改真实点文件仍需决策节点 3 逐项批准，本任务只落代码与 fixture 测试）。

## [2026-11-22T01:05:00+08:00] evidence: M4-3 proxy-attach 接入改写完成
- Apply 增 Options.ProxyAddr：proxy-attach 类改写时同步改 base_url——litellm yaml 的 api_key 行被替换后自动在其块内补/改 api_base: http://127.0.0.1:<port>/v1；JSON env（claude settings 形态）的 *_TOKEN 值替换为占位符后，同块 *_BASE_URL 兄弟键改写为 http://<addr>。
- 集成测试：fixture 改写后 litellm 含 api_base 指向 127.0.0.1:8787/v1、claude settings ANTHROPIC_BASE_URL=127.0.0.1:8787、全部改写文件零明文；再起真实本地代理（随机回环端口 + mock 上游，key 从被改写的 ref 读取），以 OpenAI 客户端语义 POST /v1/chat/completions 完成 200 调用（上游校验注入的 Bearer 即被迁明文）。-count=2 稳定，全仓 10 包 ok，vet/gofmt 干净。
- .zshrc export 行注释化 + agentvault 提示已在 M4-2 覆盖（含 agentvault run 恢复指引），本条测试复用同一 fixture。

## [2026-11-22T01:25:00+08:00] evidence: M4-4 agentvault run 环境注入完成
- cmd/agentvault/run.go：run --env VAR=ref:provider/key（可重复）-- <cmd>：进程内从钥匙链解密（service 命名空间复用配置），注入子进程 env；stdio 直通、信号转发、子进程退出码透传。
- 注入语义：父进程环境被剔除同名变量后叠加注入值——父环境构造上不含明文；spec 格式错误或 ref 不存在直接拒绝（不启动子进程）。
- 测试：子 env 含注入值且父环境零明文、坏 spec/缺失 ref 拒绝、printenv 子进程看到准确值。-count=2 稳定，全仓 10 包 ok，vet/gofmt 干净。
- 真机冒烟：`agentvault run --env MY_GLM=ref:bigmodel/primary -- sh -c 'echo len/prefix'` → len=49 prefix=f693（钥匙链真实 key 经 run 注入成功，输出仅掩码）。

## [2026-11-22T01:40:00+08:00] evidence: M4-5 迁移报告完成 — M4 代码全部就绪
- migrate.WriteReport：三分类汇总（每类逐处列出 文件:行 变量 掩码值）+ 已入钥匙链 ref 清单 + run-inject 类指引（一工具一专用 key 录入 / agentvault run wrapper / codex env 块改写示例 / 吊销演练四步）。输入是 Finding（构造上仅掩码），报告不可能含明文。
- CLI：agentvault import --report 输出报告（--report 与 --yes 可叠加：先应用再报告）。
- 测试：三分类段落、零明文断言、指引与 ref 清单存在。全仓 10 包 ok，vet/gofmt 干净。
- 真机试运行（只读）：报告输出 11 处——proxy-attach 3（litellm：GLM key ×2 已在钥匙链、deepseek ×1）、run-inject 8（.zshrc ARK ×1、codex toml ×7）、compliant 0；报告无明文。
- **待决策**：改写真实点文件（~/.zshrc、~/.config/litellm/config.yaml、~/.codex/config.toml）属决策节点 3，需用户逐项批准后方可 import --yes；批准后重跑 scan 应 0 命中即 M4 清零复核。

## [2026-11-24T15:52:00+08:00] evidence: M4 真机迁移第 1/3 步 — ~/.zshrc 改写完成
- 前置增强：BuildPlanFiltered + `import --only <file>`（按文件批准闸门，commit 4b02b41），过滤测试断言只动目标文件、其余文件明文原样保留。
- 执行：`agentvault import --home ~ --only ~/.zshrc --yes` → ref:ark/ark_api_key 入钥匙链；原行替换为含 agentvault run 恢复指引的注释；备份 ~/.zshrc.bak-20261124-155145。
- 验证：改写后文件 0 明文命中；`agentvault run --env ARK_API_KEY=ref:ark/ark_api_key -- sh -c …` 输出 len=46 prefix=ark-（与原值掩码一致）；预览模式确认计划只含该文件。

## [2026-11-24T16:05:00+08:00] evidence: M4 真机迁移第 2/3、3/3 步 — codex 与 litellm 改写完成，清零达成
- 用户确认 litellm 已不依赖（查证：usage.db 最后请求 11-17、fallback 告警最后 11-17、4000 端口连接数 0、当前会话经 agentvault 8787）且不再用 codex 明文配置，批准两文件改写。
- 执行：import --only ~/.codex/config.toml --yes（7 处入钥匙链，值全改占位符）；import --only ~/.config/litellm/config.yaml --yes（4 处入钥匙链 + 占位符；config 在 15:45 被用户新增过一个 coding endpoint，扫描为 4 处而非 3 处）。
- litellm 停用：launchctl bootout ai.zhezhu.litellm（进程 0、4000 端口无监听连接；plist 保留可恢复）。
- **发现并修复两处潜在 bug**（commit 待记）：① ref 去重只对第 2 次出现加文件后缀，第 3+ 次与第 2 次撞 ref 互相覆盖（本次 litellm 3 个 GLM 同值侥幸无害）→ 改为文件后缀 + 次数计数器，回归测试断言同文件三个同名 var 得到三个不同 ref 且各自值正确；② ensureYAMLBase 只向后找 api_base，litellm 块中 api_base 在 api_key 之前则漏改 → 增加向前搜索且保留原行缩进，回归测试覆盖。
- 验证：codex config.toml 7 处占位符、明文 0 命中；litellm config api_key 全占位符；`agentvault scan` → **no plaintext keys found（11 处清零，M4 清零复核达成）**。
- 备注：litellm 的 api_base 未指代理——文件块内 api_base 在 api_key 之前，本次改写时旧逻辑漏改且该文件已随 litellm 停用而失效，无需补改；bug 已修复供后续使用。

## [2026-11-24T16:10:00+08:00] milestone: M4 里程碑达成 — 项目最终验收通过
- 清零复核：真机 `agentvault scan` 输出 no plaintext keys found（迁移前 11 处 → 0）。
- 三分类真机落地：proxy-attach（claude settings 11-20 / litellm 11-24 改写）、run-inject（.zshrc 注释化 + agentvault run 实测注入、codex toml 占位符）、compliant 0。
- 附带处置：litellm LaunchAgent 停用（空转 7 天、零连接，plist 保留可恢复）；GLM 额度感知/兜底链路随之退役，替代方案为 agentvault quota + usage（后续可演进）。
- 全程无明文泄漏：所有输出掩码、钥匙链经 apply 入库、备份含原文（预期内）。
- Issue #2 四个里程碑（M1 保险箱核心 / M2 首次上线 / M3 流量控制 / M4 明文清零）全部完成。

## [2026-11-24T16:25:00+08:00] evidence: litellm 退役收尾清理完成
- ~/.claude/CLAUDE.md 重写：兜底感知段（fallback-alerts.log/gate.log 链路）替换为 AgentVault 网关段（127.0.0.1:8787，密钥钥匙链）；GLM 周额度段路径 ~/.config/litellm/glm-quota.json → ~/.config/llm-schedule/glm-quota.json（unit=6 逻辑保留），补 agentvault usage/provider quota 观测提示。
- ~/.zshrc 死 ANTHROPIC 块（7-14 行：ark base_url / ANTHROPIC_AUTH_TOKEN 引用 / 三模型别名 / API_TIMEOUT_MS）整块移除，备份至 cleanup-backup-20261124-160416/zshrc.bak。
- 验证：干净 login shell（env -i zsh -ic）不再导出 ARK_API_KEY / ANTHROPIC_BASE_URL（后者由 settings.json 按会话注入）；`agentvault scan` 仍 no plaintext keys found；glm-quota-check.sh 改写后已在 16:04 实跑成功（usagePct=9, level=max）。
- 保留：codex-ark/codex-glm alias、glm-quota-check.sh；dashboard-gen.py 留档不管。

## [2026-11-25T16:00:00+08:00] evidence: M5 网关并发控制与排队完成（item 22）
- 背景：GLM coding plan 实测约 3 个在途请求即上游 429（错误码 1302，官方建议请求队列/并发池）；原 pick() 只轮询+冷却不看在途数。
- internal/proxy/gate.go：provider 级账户并发闸门（同 provider 所有 key 共享上限，容量不随 key 数翻倍）+ 每 provider FIFO 等待队列；release 把槽位直接移交队首（inFlight 不变），超时/取消的 waiter 从队列摘除不占位，已获移交的迟到 waiter 主动归还槽位（防移交竞态泄漏）。
- router.acquire(ctx, model, queueTimeout)：冷却过滤后按 rr 顺序逐 provider tryAcquire，满则全跳；全满时在 rr 首选 provider 排队；状态区分 ok / no-candidate / queued-timeout / cancelled，HTTP 层分别映射原有 404/冷却 429、新 queued-too-long 429（含 Retry-After）、静默返回。pick() 保留为不排队薄壳（测试/兼容）。
- 槽位释放：forward / forwardResponses 每次尝试包进闭包 defer release()——成功路径在 flushCopy 完整回写并记录 usage 之后才出闭包（流式期间持续占位），failover/上游错误/配额拒绝/取 key 失败路径全部释放，构造上不泄漏。
- 配置：migration 6 providers.concurrency（存量行默认 3 = 实测 GLM 上限，0=不限流）；provider add --concurrency（默认 3）、provider set-concurrency <id> <n>；serve --queue-timeout（默认 120s，daemon re-exec 透传）。
- 观测：/admin/routing 每 provider 增 in_flight/queued；agentvault status 透出两列。
- 测试：闸门单测 5 例（超限跳过姊妹 provider / FIFO 次序 [0 1 2] / 排队超时 / 客户端取消不占位且计数不坏 / 槽位在流式回写后才释放）；集成测试：账户上限 2 + 同账户两把 key，8 并发请求上游实测峰值并发 ≤2 且全部 200；admin 端点断言 1 in-flight + 2 queued；queue-timeout 端到端 429 含 "queued too long" 文案与 Retry-After；provider store 并发列测试（存量行默认 3 / 显式 0 / upsert / ErrNotFound）。
- 顺手修复 messages_test 的存量 data race（records 无锁 append/poll），全 proxy 包 -race -count=2 绿。
- `make lint`（gofmt/vet/go test 全仓 10 包）全绿；沙箱 CLI 冒烟：set-concurrency 5→0→show 显示 unlimited，serve --queue-timeout 30s 启停正常。draft PR 提出；按指示不合并、不 make deploy。

## [2026-11-25T17:30:00+08:00] checkpoint: M6-A 开工（auto 模式决策层核心骨架，item 21）
- 设计依据：owner 批准 2026-11-25 的 auto 模式设计稿 v1.3（~/.orca/bots/bot-2/handoffs/auto-mode-design-draft-v1.md），原文拷入 docs/design/auto-mode.md（不改内容，头部加批准标注）。
- 三件套更新：plan.md 增 M6 里程碑节（M6-A=M6-1/2/3、M6-B=M6-4/5、M6-C=M6-6/7/8、M6-9 校准期，每块过决策节点 2）；checklist.yaml 追加 M6-A-0..3（只追加）。
- 本块范围：M6-1 plans 表（migration 7 + agentvault plans CLI）、M6-2 internal/auto 骨架（三入口别名 + 判档规则 v1 表驱动 + usage decision 字段落库）；M6-3 配额感知属后续，本块 auto 只做 GLM 档内选型。
- 红线复述：钥匙不落盘不进日志；不新增第三方依赖；migration 只追加；显式别名直通行为回归不变；:8788 冒烟不碰 :8787 生产；PR 不自合。

## [2026-11-25T18:40:00+08:00] evidence: M6-A 完成（M6-1 plans 表 + M6-2 internal/auto 骨架，PR #9）
- M6-1：provider migration 7（只追加）plans 表 provider/plan_tier/window_type/limit_value/remaining/refreshed_at/verified_at，主键三元组；SetPlan upsert / ListPlans / GetPlan(ErrNotFound)；CLI `agentvault plans set|list`（--used-pct 换算、verified>30 天标 verify 提醒）。TestPlanLifecycle 覆盖 upsert/多窗口/NotFound。
- M6-2：internal/auto 独立包——三入口 auto/auto-deep/auto-bg（不建 models 行、candidates() 拒绝、known() 专门分支 true、/v1/models 展示层追加 owned_by=agentvault）；判档规则 v1 表驱动 DefaultRules（thinking-on > alias-haiku > tools-present > small-max-tokens > no-tools > default，全部归 routine 兜底），deep 不降档由优先序构造保证并有单测；ExtractFeatures 双 wire 提取 thinking（anthropic thinking.type=enabled / reasoning_effort≠none / reasoning.effort），不读 system；目标表数据驱动 deep→glm-5.4（长上下文阈值 180k tokens 走 DeepLongContext，注释注明设计稿 [1m] 记法按脚注理解、目标暂同 glm-5.4）、routine/bg→glm-5.3-flash；pinned 入口跳判档但保留长上下文目标与观测。
- usage.Record 增 task_tier/matched_rule/decision_reason/via_manual 落库（usage 列幂等追加，无版本表），overflow/cache_rebind_cost/off_peak 留空位；forward/forwardResponses 全部 recordUsage 站点经 recordUsageCtx 统一打标，429 路径同样可审计；显式别名 matched_rule=explicit-alias、via_manual=true，其余字段零值。
- 验证：go test -count=1 ./... 10 包全绿（新增 auto 7 组单测、proxy 集成 4 例、provider/usage store 测试）；make lint（gofmt/vet/test）绿。
- Dev 冒烟：:8788 临时实例 + 本地 mock 上游——auto+thinking→glm-5.4、auto→glm-5.3-flash、auto-deep→glm-5.4、auto-bg（带 thinking 仍钉 bg）→glm-5.3-flash、显式 glm-5.4 直通原名；usage JSON 五条 task_tier/matched_rule/via_manual 全对；:8787 生产全程未触碰、未 make deploy。冒烟用一次性假 key，验证后已 `security delete-generic-password` 删除（复核 dump-keychain 无残留）。
- PR：draft https://github.com/zhezhe-star/AgentVault/pull/9 （标题注明 M6-A），不合并、不部署，继续盯 checks/review。
- 待后续块：M6-3 配额感知（隐藏接口客户端 + glm-quota.json 兜底 + 推断）、M6-6 owner 决策通道（NeedsOverflow 恒 false 至其落地）；M6-B/M6-C 块边界见 plan.md M6 节。
