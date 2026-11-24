# AgentVault

AgentVault 用于集中管理 AI 应用和 Agent 所依赖的第三方模型服务商 API 密钥，目标是降低密钥泄露风险，并在密钥已经泄露或疑似泄露时支持快速吊销、切换和恢复。

## 项目背景

2026 年 10 月，开源 AI 编程助手 OpenCode 发生一起第三方服务商 API 密钥泄露事件。据报道，一套处于停用流程但尚未彻底关停的遗留系统遭到未授权访问，Console 工作区中托管的第三方服务商 API 密钥可能已经暴露，受影响用户约占 OpenCode Console 用户的 1.4%。部分用户在收到官方通知前，已经先收到 OpenAI、Anthropic、Google Cloud 等平台的异常用量提醒或高额账单。

该事件暴露出集中托管 API 密钥的核心风险：平台为了提供路由、计费、审计等能力，将多家服务商密钥汇聚到一个控制面；一旦该控制面被攻破，密钥会被一次性放大成多个用户账户的资金损失。同时，已经“准备下线”但仍保存凭据的系统会继续构成攻击面。真正有效的止损方式也不是修改平台密码或在客户端删除配置，而是到服务商控制台吊销旧密钥、签发并替换新密钥。

完整的背景记录见 [`docs/PROJECT_BACKGROUND.md`](docs/PROJECT_BACKGROUND.md)。

## AgentVault 要解决的问题

- **密钥集中但可隔离**：不同应用、Agent、环境、工具可以使用独立凭据，避免一把密钥跨多个系统复用。
- **最小权限与用量可观测**：为每把密钥绑定明确用途和消费上限，跟踪异常请求量与费用增长。
- **泄露后的快速切换**：当密钥泄露或疑似泄露时，支持一键禁用旧密钥、切换到新密钥，并让依赖方自动恢复。
- **下线不残留风险**：系统或工作区退役时，强制执行凭据失效、访问关闭和数据清理，并保留可审计记录。

## 快速上手

AgentVault 是一个**本地**密钥保险箱 + 回环代理：API key 只存 OS 钥匙链（macOS Keychain 等），数据库里只有引用；AI 客户端指向本地代理，代理在内存中解密并注入真实密钥。明文 key 不落任何配置文件。

### 1. 安装

```bash
git clone https://github.com/zhezhe-star/AgentVault
cd AgentVault && go build -o bin/agentvault ./cmd/agentvault
export PATH="$PWD/bin:$PATH"
```

### 2. 录入 provider 与 key（key 不落盘，进 OS 钥匙链）

```bash
agentvault provider add bigmodel \
  --base-url https://open.bigmodel.cn/api/paas/v4 \
  --anthropic-base-url https://open.bigmodel.cn/api/anthropic

# 管道输入，不回显、不进 shell history：
printf '%s' "$MY_GLM_KEY" | agentvault add-key bigmodel --key-id primary
agentvault provider add-model bigmodel glm-5.3-flash --key-id primary
```

### 3. 启动代理（仅绑定 127.0.0.1）

```bash
agentvault serve --daemon   # 127.0.0.1:8787
agentvault status           # pid / 冷却 key / 熔断 key / provider 计数
```

**本地构建与部署纪律**（Makefile 已固化）：

- `make deploy` —— 生产升级，**只允许在 main 主 checkout 执行**（非 main 分支会被拒绝并退出）。构建 → 替换 ：8787 的存活 proxy → 打印 status。
- `make serve-dev` / `make stop-dev` —— 开发分支/worktree 专用：测试端口 `:8788` + 独立数据目录 `~/.config/agentvault-dev`（首次自动从生产库播种，钥匙链全局共享故无需重录 key），绝不触碰生产实例。
- 日常验证跑 `make lint`（fmt + vet + test）；不要在开发 worktree 手工对 :8787 执行 `serve --daemon`。

### 4. 接入客户端（OpenAI 或 Anthropic 协议均可）

OpenAI 协议（如各 CLI 的 `OPENAI_BASE_URL`）：

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
export OPENAI_API_KEY=agentvault-local-placeholder   # 占位符，真 key 在钥匙链
```

Anthropic 协议（如 Claude Code 的 `~/.claude/settings.json`）：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "ANTHROPIC_AUTH_TOKEN": "agentvault-local-placeholder"
  }
}
```

代理按请求体里的 `model` 字段路由到对应 provider/key，透传请求与响应（含 SSE 流式），只替换凭据头。

### 5. 用量观测与配额

```bash
agentvault usage --model glm-5.3-flash --since 1h   # 按/provider/key/模型/时间过滤
agentvault usage --json --limit 20
agentvault provider quota bigmodel --scope provider --window 1h --max-tokens 100000
# 滚动窗口超限后的下一请求返回 429（agentvault_quota_exceeded），并记入 usage
```

控制台：浏览器打开 <http://127.0.0.1:8787/>，三级视图 provider → key → 模型（累计真实 token 数）。

### 路由与容错

- 多 key 按 weight 轮询；上游 429/5xx 时该 key 冷却并同请求内 failover。
- 连续 3 次失败触发熔断（默认 5 分钟），到期后半开探测，成功闭合、失败重开。
- 所有请求（含 429/上游失败）每调用一条 usage 记录，跨重启持久化。

### 安全模型

- key 明文只存在于两处：OS 钥匙链、代理进程内存（TTL 缓存 5 分钟）。
- SQLite 仅存 `secret_id` 引用；AES-256-GCM 信封加密数据加密密钥（DEK）本身也在钥匙链。
- 代理仅绑回环地址；日志在构造上不含 header/body/key。
- `leakscan` 测试将合成 key 走完整管线后扫描 DB 字节、源码树与输出，断言零泄漏。

设计细节见 [docs/design/architecture.md](docs/design/architecture.md)，同类项目调研见 [docs/design/survey.md](docs/design/survey.md)。
