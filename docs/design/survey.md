# 同类项目调研：可借鉴点与本项目取舍

M3-5 产出。调研对象是「多 provider 密钥管理 / LLM 网关」方向的主流开源项目，结论用于确认 AgentVault 的自实现边界：哪些语义值得借鉴，哪些依赖不该引入。

## new-api（QuantumNous/new-api，Go）

定位：多租户 LLM 网关与计费平台（one-api 的活跃分叉），Web 控制台、渠道分组、按 token 计费、用户配额体系。

- **可借鉴**：渠道（channel）≈ provider+key 的建模；失败渠道自动禁用再定时探活的思路与本项目冷却/熔断语义一致；按模型聚合 token 用量的展示维度被 M3-6 采用。
- **取舍：不引入**。它是面向部署服务的平台（自带用户体系、Redis、MySQL 依赖），而 AgentVault 是单用户本地工具。引入即意味着把「本地保险箱」变成「自建云网关」，与明文零落盘、零外部服务的前提冲突。

## one-api（songquanpeng/one-api，Go）

定位：new-api 的上游，统一 OpenAI 格式转发到多家上游。

- **可借鉴**：统一网关入口、按 model 字段路由的协议形态（本代理 `/v1/chat/completions` 的路由键一致）。
- **取舍：不引入，且不照搬其「协议转换」层**。one-api 的核心价值是把各家 API 转成统一 OpenAI 格式；AgentVault 的代理是**透明透传**（只注入凭据头，body 原样转发），因此不需要转换层。Anthropic 协议透传（M2-7a）自实现约 50 行，无依赖。
- 附带风险：one-api 要求把上游 key 明文存它的 MySQL——正是本项目要消除的形态。

## bricks-llm（Go 库）

定位：Go 生态的 LLM provider 抽象库，多 provider 客户端封装与 fallback。

- **可借鉴**：provider 抽象接口的分层方式；fallback 链式重试语义（本项目 failover 更简单：候选列表顺序 + 冷却）。
- **取舍：不引入**。它是客户端 SDK 抽象（为应用开发者统一调用接口），而 AgentVault 是网络代理（客户端无感知）。场景不同，引入只会带来一层用不到的抽象。Go 官方 `net/http` + 自定义 wire 结构（upstreamPath + setAuth）已足够，且双协议 usage 解析（openai/anthropic 字段名不同）本来就要自己写。

## LiteLLM（Python）

定位：LLM 网关 + SDK 双形态，100+ provider 统一，proxy 模式支持 key 管理、预算、路由。

- **可借鉴（语义层）**：budget/滚动窗口配额的语义（per-key 与 per-team 两个作用域——对应本项目 scope=provider/key）；router 的 cooldown/allow-fails 参数直接启发了 BreakerThreshold=3、冷却 60s 的默认值。
- **取舍：不引入运行时**。kickoff 决策（2026-11-17）已定 Go 技术栈；LiteLLM 是 Python 服务，引入即多一套解释器与依赖树，与「单二进制、零外部服务」目标冲突。它也要求上游 key 存其 DB（支持加密但默认明文）。
- 用量记录里的双协议 SSE 末 chunk 解析（OpenAI `usage` in last chunk vs Anthropic `message_delta`）参考了其 usage 兼容层的字段映射思路。

## 结论：自实现边界

| 能力 | 来源 |
|---|---|
| /v1/chat/completions、/v1/messages 透传 + 凭据注入 | 自实现（~100 行，wire 抽象） |
| 轮询 + 冷却 + failover | 自实现，借鉴 one-api/LiteLLM router 语义 |
| 熔断 + 半开探测 | 自实现（经典 circuit breaker，阈值/时长借鉴 LiteLLM 默认） |
| 滚动窗口配额（读真实 usage_records） | 自实现，scope 语义借鉴 LiteLLM budget |
| 双协议 token 抽取（JSON+SSE） | 自实现，字段映射参考 LiteLLM |
| usage 存储 | SQLite（modernc.org 纯 Go 驱动），唯一非标准库运行时依赖之一 |

引入的外部依赖仅三个：cobra（CLI）、zalando/go-keyring（OS 钥匙链）、modernc.org/sqlite（纯 Go SQLite，免 cgo）。没有任何 LLM 网关类依赖——本项目与它们是竞争关系而非上下游。
