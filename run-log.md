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
