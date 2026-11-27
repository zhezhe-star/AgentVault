<!-- owner 批准 2026-11-25, item 21 -->

# AgentVault 网关 auto 模式设计稿（v1 初稿，待 owner 评审）

2026-11-25 · 大猫咪（方案调研设计负责人）综合：智谱/DeepSeek 计费调研、开源实践对标、辣条 6 问共识、owner 决策 e226。

**v1.1（2026-11-25，owner 决策 e239）**：溢出策略裁定选 **A**——所有 auto→DeepSeek 切换（含后台档）一律先抛 owner 确认，网关无自动溢出路径。§5a/§5/§13/§14 已按此落稿。

**v1.2（2026-11-25，owner 反馈 e244）**：新增 §3a「auto 与手动选择的关系」——默认状态即 auto（客户端默认模型配 auto），手动点名别名是兜底/逃生通道；思考深度在 auto 下作档位信号、手动下透传。

**v1.3（2026-11-25，owner 反馈 e247）**：§3a 补「Orca/Claude Code 具体接入方式」——槽位经 `ANTHROPIC_DEFAULT_*_MODEL` 映射到 **auto 档位入口家族**（auto / auto-deep / auto-bg），保住客户端槽位意图；§7 通道形态裁定三面全做（CLI + Console + 事件文件）。

## 1. 目标 / 非目标

**目标**：客户端请求虚拟模型名 `auto` 时，网关根据「任务档位 + 套餐余量 + 时段 + 缓存亲和 + 并发/冷却状态」自动选择具体上游模型（GLM 套餐优先、DeepSeek 按量兜底），在满足开发质量的前提下最小化 token 成本。

**非目标（v1）**：
- 不做语义/LLM 评分路由（OpenRouter 式 11 维评分退化为规则层；必要时 v2 再评估）
- 不做多账号池化（自有订阅，无 sub2api 场景）
- 不做自动溢出 DeepSeek 的「无人审批」（owner 决策 e226：溢出前必须抛 owner）
- 不做价格自动抓取（人工表 + verified_at 提醒）
- 不改变显式点名别名的直通语义

**验收口径**：显式别名行为与现在完全一致；`auto` 请求可观测（决策记录可审计）；规则可数据化调整；误判兜底为「判不出走 GLM 常规档」。

## 2. 事实基础（调研结论，全部 2026-11-25 核实）

| 维度 | GLM coding plan | DeepSeek API |
|---|---|---|
| 计费 | 积分制双闸门（5h + 周）；系数表（5.4: 4.6/1.1/16）；MCP ×1.2 | 预充值按 token；峰谷 5 抓（北京周一至五 9-12/14-18 高峰） |
| 错峰 | 非高峰（周一至五 14:00-18:00 UTC+8 之外，含周末）积分 5 折 | 空闲（其余全部时间，含周末节假日）5 折 |
| 并发 | 动态调整不公开（Max>Pro>Lite），1302 官方建议排队 | 公开固定 flash 2500 / v4-pro 500，可免费扩容 |
| 缓存 | 命中系数约 1/4（5.4: 1.1 vs 4.6） | 命中价 = 未命中 2%-3.3%（30-50 倍价差） |
| 余量查询 | 隐藏接口 /api/monitor/usage/quota/limit（percentage+nextResetTime，个人版可用；团队版 bug 空返回） | /user/balance（is_available + 余额明细） |
| 限制 | Key/endpoint 强绑定（错绑 1113）；GLM-5.4 仅套餐渠道 | 1M 上下文/384K 输出；usage 含 cache hit/miss 字段 |
| 重置 | 周重置卡每周 2 次（owner 手动） | 充值即恢复 |

**开源对标结论**：① 业界无「订阅余量作一等调度信号」的现成实现（自研差异化点）；② 峰谷路由侧无网关实现（LiteLLM 仅有计费侧 off_peak_pricing，其窗口语法 hours_utc+weekdays+IANA 时区可移植）；③ 缓存亲和有成熟范式（LiteLLM 自动识别 `x-claude-code-session-id` 等会话头；OpenRouter「宁调 reasoning-effort 不换模型」）；④ 骨架学 new-api 渠道优先级+失败降级（规避 issue #6571 同渠道重复重试）；⑤ 决策分层学 OpenRouter/CCR：规则层优先、可解释、确定性。

## 3. 总体架构

`auto` 是 **handler 前置的决策层**，不注册为普通 models 行（否则会被 candidates() 当普通候选进轮询、绕过判定）：

```
客户端请求 model="auto"
  → server.forward 识别 auto
  → auto.Decide(ctx, 请求特征, 状态快照)   // internal/auto 独立包
      输入: 别名语义/thinking/tools/max_tokens/会话绑定/套餐余量/时段/冷却
      输出: 目标模型别名 + 决策理由 + 是否需 owner 确认
  → (需确认时) 写 pending_decision，回 429 + Retry-After + 决策 ID   // 见 §7
  → 改写 model 字段 → 复用现有 acquire/candidates/pick/门闸/冷却链路
  → pick 之后按 candidate/provider 做最终 model 改写（修 upstreamName 首匹配坑）
  → usage.Record 增加 decision 字段（档位/命中规则/目标/溢出原因/缓存代价）
```

**原则**：显式别名直通不变；`auto` 判定失败一律归 GLM 常规档（fail-safe）；规则全部数据化（表驱动，可热调）。

## 3a. auto 与手动选择的关系（owner 方向 e244：默认 auto，手动兜底）

**定位**：
- **auto = 默认路径**。客户端（Orca/Claude Code）的默认模型配置为 `auto`，常规流量全部进决策层；思考深度（thinking/reasoning 参数）作为档位信号参与判定（优先级 1）。网关 /models 暴露 `auto` 供客户端选为默认。
- **手动点名别名 = 兜底通道（逃生舱）**。客户端 /model 切到具体模型（如 glm-5.4）时，网关保持现直通语义——这正是兜底的实现方式：auto 误判后人工纠偏、特殊任务指定模型、排障对照。
- **组合表**：

| 客户端操作 | 网关行为 | 说明 |
|---|---|---|
| 默认 auto + 调思考深度 | 进决策层，thinking 参数进档位判定（deep 档） | 常态：复杂任务 |
| 默认 auto，thinking 关 | 进决策层，按别名语义/tools 判档 | 常态：后台/常规任务 |
| 手动点名 glm-5.4 | 直通（不进决策层），记 via_manual | 兜底：误判纠偏/特殊任务 |
| 手动点名 + thinking 参数 | 直通，参数原样透传 | 兜底 + 手动深度 |

**边界与约定**：
- 网关不强制「仅兜底可手动」——无法亦无须 policing；这是使用约定，靠客户端默认配置达成（默认模型=auto）。
- 手动请求照常记 usage 并打 `via_manual` 标记：两周校准期作对照数据（auto 决策 vs 手动选择），量化误判率与成本差，反哺规则热调。
- 兜底链条：auto 误判 → 手动点名纠偏（即时生效）→ 校准期数据 → 规则调参 → 手动介入递减。
- v2 备选治理项：配置 `manual_allowed`（默认 true），关闭后除 auto 外别名一律拒绝——仅在 owner 想硬性约束使用习惯时启用，不进 v1。

### 3a-1. Orca/Claude Code 具体接入方式（e247 问，实测现状后落稿）

**现状（2026-11-25 实测本机配置）**：Claude Code 经 `ANTHROPIC_BASE_URL=http://127.0.0.1:8787` 走网关；四个内置槽位用 `ANTHROPIC_DEFAULT_*_MODEL` 映射到 glm 别名——`opus→glm-5.4`、`sonnet→glm-5.3-flash`（默认槽 model: sonnet）、`haiku→glm-4.5-air`（后台/子代理轻量槽）。网关 /v1/models 现列 15 个别名。

**关键问题**：槽位映射会把「客户端选了 opus 还是 haiku」这层意图**抹掉**（请求到达网关时 model 字段已是映射后的别名）。若三个槽全映射到 `auto`，网关无从区分强档意图与后台意图。

**解法：auto 档位入口家族**——映射目标不是裸 `auto`，而是档位钉死的入口别名（内部仍走 auto 决策层的配额/确认/观测链路，只是跳过判档）：

| Claude Code 槽位 | 现映射（保持不变直至切换） | 新映射（auto 上线后） | 网关行为 |
|---|---|---|---|
| 主槽 opus（强推理） | glm-5.4 | **auto-deep** | 钉深度档：GLM-5.4 套餐内调度 + 全套观测 |
| 主槽 sonnet（默认） | glm-5.3-flash | **auto** | 自动判档（thinking 参数在这里生效：开了 thinking → deep） |
| 后台槽 haiku | glm-4.5-air | **auto-bg**（或过渡期保留 glm-4.5-air 直通） | 钉后台档：GLM 最便宜模型，溢出仍需 owner 确认 |
| /model 手动点名具体别名 | glm-5.4 等 | 不变 | 直通 = 兜底通道（§3a） |

**接入步骤（auto 上线后 owner 只需改三个环境变量）**：
1. 网关侧：/v1/models **展示层**加入 auto / auto-deep / auto-bg 三个名字（决策在 forward 识别，不建 models 行、不进轮询池——known() 与 candidates() 对这三个名字走专门分支）。
2. `settings.json` 的 env 三行：`ANTHROPIC_DEFAULT_OPUS_MODEL=auto-deep`、`ANTHROPIC_DEFAULT_SONNET_MODEL=auto`、`ANTHROPIC_DEFAULT_HAIKU_MODEL=auto-bg`。
3. Orca 侧模型选择：Orca 起会话时的模型注入落到 Claude Code 的 model 参数（最终经槽位映射发往网关）——若 Orca 的选型列表来自网关 /v1/models 则自动出现 auto 家族；若是静态清单则在 Orca 配置手动填 `auto`。**M6 实施时先实测 Orca 的模型列表拉取链路再定第 3 步写法。**
4. 切换顺序：先在 owner 本人一个测试 chat 上验证全链路（判档/观测头/手动兜底），再逐步切日常 chat 与 worker 默认值。

**子代理**：Task 工具的 model 字段（opus/sonnet/haiku/inherit）经同一套槽位映射生效——子代理请求天然带档位意图（haiku 子代理 → auto-bg），无需额外适配。

## 4. 档位判定（辣条共识）

信号优先级：
1. **thinking/reasoning 参数**（最强：显式深度思考 = 强档）
2. **模型别名语义**（haiku 类 = 后台档；Claude Code 后台任务固定走 haiku 类别名，可靠）
3. **tools 结构**（无 tools 的纯文本请求偏向后台/常规）
4. **max_tokens / 上下文长度**（弱信号）

不使用 system 提示特征（误判率高、客户端耦合）；不使用 streaming 与否（不可靠）。

档位输出：`background`（后台轻量）/ `routine`（常规）/ `deep`（深度思考）。**判不出 → routine（走 GLM）。**

黑名单：`deep` 档永不降档溢出（架构/安全/大重构靠「thinking 参数 + deep 档」规则外默认涵盖，不做语义识别——识别「这是架构设计」不可靠，识别「这是明确的后台轻量任务」才可靠）。

## 5. 路由决策流程

```
Decide():
  1. 请求点名了具体别名？ → 直通（不进 auto 逻辑，保持现有契约）
  2. 档位判定（§4）
  3. 会话绑定检查（§8）：本会话已绑定 (provider,key,model) 且健康？ → 继续用（缓存亲和）
  4. GLM 套餐余量（§6）：
     - <70%：routine/background/deep 全走 GLM
     - 70–95%：routine/deep 走 GLM；background 判为「溢出候选」，但一律先进 owner 确认（§7）
     - >95%：background/routine 可提请溢出（同样需 owner 确认）；deep 永不提请，走 GLM 直至硬限
  5. GLM 冷却/并发状态（新增 per-model 只读查询，不绕锁）：
     - 并发类短冷却（秒级，≈1302）：不溢出，auto 请求进网关 FIFO 队列等（已有 M5 闸门）
     - 周额度/长冷却（≈1310/quota）：**抛 owner**（e226），见 §7
  6. 深度任务（deep）在 GLM 可用时永远走 GLM；GLM 不可用时抛 owner，不静默换 DeepSeek
```

**5a. 溢出策略（已裁定：owner 决策 e239 = A）**：所有 auto→DeepSeek 切换——无论档位、无论余量分档——一律先写 pending_decision 抛 owner 确认，**网关无任何自动溢出路径**。auto 的自动部分只剩「判档 + GLM 套餐内选型 + 缓存亲和 + 错峰标记」；DeepSeek 是需要 owner 批准的溢出目标。设计后果：
- pending_decision 通道（§7）从边缘路径升级为溢出唯一入口，是 v1 核心组件
- 每日预算上限保留为防线（防批准后用量失控），不再是主要闸门
- 辣条主张的 background 自动溢出（B）未采纳；若两周校准期数据显示逐次确认负担过重，可再议

## 6. 配额感知

三层信号，主备降级：
1. **主**：GLM 隐藏接口 `GET /api/monitor/usage/quota/limit`（Authorization: 套餐 Key）→ 5h 窗口 percentage + nextResetTime；每 5 分钟轮询 + 指数退避；**接口属内部 API，失败降级到 2**
2. **备**：本地 `~/.config/llm-schedule/glm-quota.json`（unit=6 周额度）灌入 plans 表
3. **兜底推断**：usage store 观测（429 频率滑动窗口、积分消耗速率反推）——对不公开数值的并发维度只能这样做

DeepSeek：`GET /user/balance` 的 is_available 作硬开关；按量渠道无「余量软分档」需求，只设每日预算上限（防悄悄烧钱，辣条建议）。

plans 表（新表，不动 Model 结构）：provider、plan_tier、窗口类型（5h/周）、上限、当前余量、刷新时间、verified_at（价格/系数人工核对时间，超 30 天 console+CLI 提醒）。

## 7. Owner 决策通道（e226 落地机制）

网关是本地守护进程，owner 就在机器旁，用** pending_decision 表 + 三个触达面**：
1. `agentvault auto pending` CLI：列出待决溢出（请求特征/档位/原因/预估 DeepSeek 成本），`agentvault auto approve <id|all>` / `deny`
2. Console（:8787/）加 pending 卡片
3. **主动通知**：网关写事件文件到固定路径（Orca/自动化可监听），配合 owner 已有的自动化习惯

决策超时策略：pending 未决时该请求按「不溢出」处理（排队等 GLM 或按配额语义失败），不阻塞其他请求。

**形态已裁定（owner 决策 e247）：三面全做**——CLI（批准/拒绝操作入口）+ Console 卡片（可视化）+ 事件文件（主动通知，供 Orca 自动化监听提醒）。优先级：事件文件让 owner「知道有事」，CLI 完成决策动作，Console 作补充视图。

## 8. 缓存亲和（对标 OpenRouter/LiteLLM，本项目收益 30-50 倍价差）

- 识别会话头：`x-claude-code-session-id`（Claude Code）、OpenCode `x-session-id`（子 agent 父 ID 优先）、通用 `session-id`
- 维护 session → (provider, key, model) 绑定表（内存 + TTL 2h，断线不持久化）
- **会话内粘性原则**（OpenRouter）：能调参解决的不换模型；能换 key 解决的不换 provider；auto 改判的代价函数必须含「缓存重置成本」（换绑 = 前缀缓存全失，DeepSeek 侧等于该会话输入价 ×30-50）
- 绑定失效条件：目标不健康（冷却/熔断/额度耗尽）或会话超 TTL

## 9. 错峰调度（自研差异化点）

- 配置 `off_peak_windows` 移植 LiteLLM 语法：hours_utc + weekdays + IANA 时区 + 跨午夜 + 错配 fail-safe 回标准档
- GLM：非高峰 = 周一至五 14:00-18:00 UTC+8 之外（含周末）；DeepSeek：空闲 = 周一至五 9-12/14-18 UTC+8 之外
- 路由侧用法 v1：闲时窗口内放宽 GLM 并发保守估计（官方明示低峰动态提升并发权益）；错峰标记进 usage/响应头
- v2 预留：可延迟任务队列（批量总结/索引类后台任务挪闲时执行）
- ⚠️ 两家高峰窗口错位（GLM 高峰=工作日下午，DeepSeek 高峰=工作日上午+下午大半），叠加错峰调度时避免「GLM 高峰挤到 DeepSeek 高峰」的反向放大

## 10. router/gate 改造清单（辣条指出的坑，全部采纳）

1. router 增加 per-model 只读冷却查询（锁内快照，不绕锁直读 map）
2. model 别名改写**下沉到 pick 之后按 candidate/provider 做**（修 upstreamName 首匹配歧义）
3. auto 二次决策路径：目标别名 pick 失败（全冷却）→ 回 Decide 重判 → 改写 → 再 pick，保住 known() 的 404 vs 429 语义
4. gate（M5 已上线）继续承担账户级并发闸门；auto 不绕过它
5. internal/auto 独立包 + plans 独立表，不污染 Model 结构

## 11. 可观测性

- usage.Record 增字段：task_tier、matched_rule、decision_reason、overflow(bool)、cache_rebind_cost、off_peak(bool)、via_manual(bool)
- 响应头（学 CCR）：`x-agentvault-route`（实际 provider/model）、`x-agentvault-decision`（档位+理由缩写）、`x-agentvault-off-peak`
- /admin/routing 增 auto 面板：各档位请求数/溢出数/pending 数/估省成本
- 请求日志（仅元数据，延续无 body/key 的构造性脱敏）

## 12. 里程碑建议（M6，实施前需 owner 批准计划 = 决策节点 1）

| # | 内容 | 验收 |
|---|---|---|
| M6-1 | plans 表 + 价格/系数人工表 + verified_at | migration + CLI 灌数 |
| M6-2 | internal/auto 决策层骨架（档位规则 v1 + 黑名单 + decision 记录） | 单测覆盖规则表；判不出归 routine |
| M6-3 | 配额感知（GLM 隐藏接口客户端 + json 兜底 + 推断） | 接口失败自动降级；软分档生效 |
| M6-4 | 缓存亲和（会话头识别 + 绑定表 + 代价函数） | 换绑决策记录含缓存成本 |
| M6-5 | router 改造（§10 五项） | 现有别名行为回归不变 |
| M6-6 | owner 决策通道（§7，溢出唯一入口，核心路径） | pending/approve/deny 全链路 |
| M6-7 | 错峰窗口配置 + 闲时放宽 | 窗口语法单测（跨午夜/时区） |
| M6-8 | 观测面（字段/响应头/面板） | /admin/routing 可见 |
| M6-9 | 两周真实流量校准期 → 调参复盘 | run-log 记录数据 |

实施纪律：devflow 三件套照旧；每步 lint+tests 绿；:8788 dev 端口先验；部署上线前过决策节点。

## 13. 风险与待办

- **R1**：GLM 隐藏 quota 接口无官方承诺，可能变动/限流 → 三层降级已设计，接口失效只影响精度不影响可用性
- **R2**：Key/endpoint 绑定——现生产配置在用且正常；M6-1 灌 plans 表时核对 vault 里两把 key 的类型与 baseURL 匹配（错绑 1113）
- **R3**：档位误判 → 兜底三件套（判不出归 routine / 决策可审计 / 规则热调）+ 两周校准期
- **R4**：~~后台档自动溢出~~ 已裁定不自动溢出（e239 选 A），白名单不进 v1
- **R5**：GLM-5.4 仅套餐渠道且高峰速度仅 70-80 tok/s——SLA 敏感场景错峰收益大，但无法按量兜底，只能在套餐内调度

## 14. 评审状态（2026-11-25 e239/e244/e247 后更新）

1. §5a 溢出策略：**已裁定选 A（全部人工确认）**——已落稿。
2. §3a auto 与手动关系：**已落稿**（默认 auto，手动=兜底通道）。
3. §3a-1 接入方式：**已落稿**（槽位映射 auto 档位入口家族，含实测现状与切换步骤）。
4. §7 决策通道形态：**已裁定三面全做**（CLI + Console + 事件文件）。
5. 剩余：**待 owner 批准 v1.3 进入实施规划**（三件套立 plan）。
