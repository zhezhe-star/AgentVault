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
