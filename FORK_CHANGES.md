# Fork 差异能力记录

本 fork 相对上游主线的差异清单：哪些行为是 fork 特有的、改在哪些文件、回主线时不要恢复什么。
配置项与用法见 README「配置说明 → 字段速查」。

- **上游仓库**：`https://github.com/linguo2625469/workbuddy2api-panel`（remote `upstream`）
- **已同步基线**：`d66384d`（v1.13.0-panel）
- **共同原则**：新增能力自成一体（独立包/文件），既有代码只在少量挂载点插入调用

---

## 1. 差异能力一览

| # | 能力 | 行为差异 | 默认 |
| --- | --- | --- | --- |
| 1 | **转发契约（严格、不改写）** | 出站只做校验与编码：未知字段与大整数完整保留，无法表达的输入直接 400（跨协议入口的提示/状态字段另有「接受即丢」，见第 15 条）；删除系统提示词清洗、内容拦截降级重试、思考自动补档、GPT `max_tokens` 抬升、工具历史重排。**例外**：越界 `reasoning_effort` 按实际选中账号所属域的模型档位能力归一（回退目标可配，见第 18 条；未配置时取 ≤请求档位的最高支持档），改写进归档 `rewritten` 列可见 | 生效（无开关） |
| 2 | **响应统一管线** | 流式与非流式共用同一 SSE 解析与完成判定；HTTP 错误与 `error` 帧同一分类；按 choice index 独立聚合；错误帧/截断不再伪装成功；新增首模型事件/首生成/尾部阶段超时 | 生效 |
| 3 | **重试与账号策略** | 仅「明确未受理」才换号；已生成/已提交一律不重放；限流与配额不跨账号绕过 | 生效 |
| 4 | **系统提示词体系** | 组合位置 `none`/`replace`/`after`/`append`/`inject` + 六种内置预设（`official-*` 为官方渲染产物逐字，`default` 为自设计位）+ 按账号域覆盖 + 面板预览；预设是静态 MD 加载即用，无模板标记、无运行期变量改写；导出工具见 [scripts/render-official-presets.py](scripts/render-official-presets.py)，官方模板归档见 [docs/official-templates/README.md](docs/official-templates/README.md) | `none` |
| 5 | **出站指纹改写层** | 改写 user/assistant/tool/推理/工具入参及 tools 声明里的已知指纹串（内置 10 条：7 逐字 + 3 变长模式；按 ID 禁用黑名单 + 替换文本覆盖 + 自定义规则） | `false` |
| 6 | **分域身份与 UA** | 按 `realm × 用途` 生成版本、产品名、Origin、语言与 UA；不随代理域名变化 | CN `5.7.6`/Global `5.6.2` |
| 7 | **缓存键** | `prompt_cache_key` 改 HMAC 派生（持久化 secret）；无显式会话则不生成 | 生效 |
| 8 | **配置读写从简** | 单份 `config.json` 即唯一真相：读取只认已知键（不认识的键忽略并告警），保存 = 提交叠加到磁盘现值后按当前 schema 整份覆盖 | 生效 |
| 9 | **Web 管理面板** | 内嵌单页（明暗主题，七个视图）：账号运维 / 用量与积分 / 模型档位 / 在线改配置（热生效，唯一真相为 `internal/config/catalog.go`；仍需重启的只有 5 个字段：监听地址、状态文件路径、服务读超时、Upstash 地址与 Token）/ 运行日志 / 任务中心 | 生效 |
| 10 | **积分任务体系** | 任务列表/接受/领取 + 「一键完成」覆盖 25 个内置成长任务动作（纯 API）；任务中心全账号扫描 + 执行队列；哪些任务参与自动化可配置（见 §2） | 生效（缺省全启用） |
| 11 | **出站代理** | 普通正向代理 + Resin 粘性代理池 + 账号级代理开关 | 未配置 = 不接入 |
| 12 | **裸模型名默认域** | `model_default_realm = cn / global / auto(:cn,global) / auto:global,cn` | `cn` |
| 13 | **客户端特征对齐** | 稳定设备/会话指纹、硬件特征离散化、`/v2/report` 桌面指纹、版本自检 | 启用 |
| 14 | **面板版本配置** | 占位符来自后端内置基线（不硬编码）；「一键填入已拉取版本」；CLI 版本需人工核对 | 生效 |
| 15 | **Responses / Anthropic 桥接** | `/v1/responses` 无状态文本 / function tools；`/v1/messages` 文本 / client tools。复用原有执行、重试、用量与日志；原生 Chat 保留扩展。纯提示/遥测字段、不支持的工具声明与客户端侧推理状态接受即丢（写入请求归档并在面板展示）；近似变换显式可见（`is_error` → `[tool error]` 前缀、`stop_sequences` → Chat `stop`）；会改变语义或伪造能力的字段（`previous_response_id` / `conversation` / `background:true` / `strict:true` / 白名单外未知字段）仍明确 400。见 [兼容说明](docs/PROTOCOL_COMPATIBILITY.md) | 生效（限定子集） |
| 16 | **模型目录门禁（显式刷新制）** | 出站前先查模型目录快照：快照可用而模型不存在 → 直接 `404 model_not_found`，不选号、不打上游；命中时顺带重排候选域与纠正大小写。快照为空 → fail-open。快照只在启动预热与面板「模型能力」页手动刷新时写入；客户端请求路径零上游探测 | 生效 |
| 17 | **原生工具调用标记还原（DSML→tool_calls）** | 上游（DeepSeek 系）在请求**未声明 tools** 时把工具调用吐成全角竖线原生标记文本落进 `content`；本层在响应出站前识别并还原成结构化 `tool_calls`。流式与非流式共用同一帧管线（`consume` 内展开）。判定分两级：名单非空走严格比对，名单为空（tools 丢失常态）走弱判定（块完美闭合 + 工具名形如标识符）。硬前提：本 choice 无结构化 tool_calls（真实调用一律优先让位）、块内不夹带正文；拿不准一律原文回吐，**绝不吞字节**。属 §15 式**显式可见近似变换**：命中写入归档 `rewritten` 列（面板展示）+ 日志一行，非静默改写 | 生效（限 Chat/Responses/Messages 出站路径） |
| 18 | **模型默认思考档本地覆盖** | 模型「默认档」此前是上游/编译期口径、用户改不了；本项让配置压过它（**本地的覆盖上游配置**）：命中即改 `/v1/models` 与面板宣告的 `reasoning_default_effort`、客户端未带档位时的出站注入、越界档位的回退目标三处（同源一份表）；取值先落到该模型的支持集上（绝不宣告/发出必 400 的档位），越界值不会发出；off/none（关闭思考）拒绝作为默认档；改动进归档 `rewritten` 列 + 日志（非静默）。见 §6 | 未配置 = 完全沿用上游 |
| 19 | **模型容量队列（6020/6021/6022 + HTTP 202）** | 上游对热门/新模型会先入队再服务。此前 202 被当成 502（`upstream_queue_unsupported`）、业务码落通用 4xx（ErrClient）甚至被限流词表误判成 429 软冷却（**误罚健康账号**）。现按队列语义处理：识别 `code`/`bizCode` 结构化码与 HTTP 202，末端回 **429 + Retry-After**（`data.retry_after` 秒/毫秒两口径归一，或响应头）与带位置/容量/等待时长的 `gateway_hint`；账号**零惩罚、不轮转**（排队与账号健康无关，6020/6021 换号也不缩队）。见 §7 | 生效 |
| 20 | **派生会话身份 `X-Conversation-ID`** | 官方客户端**总会**发会话 id；第三方客户端（Claude Code / Codex / pi / OpenAI SDK）普遍不带 → 上游看不到「同一会话」，会话维度的上下文缓存/后台归集全落空。本项在「客户端没给 + 网关已算出稳定会话键」时把会话键派生为 UUIDv4 形态出站（`session.DerivedConversationID`，固定盐 → **跨重启稳定**）；只进请求头与缓存键材料，不返回客户端。默认关（A/B 后再定）。见 §7 | `false` |
| 21 | **出站形态补齐（对齐官方客户端）** | `X-Agent-Type` 恒发（入站优先，缺省按官方规则派生 `subagent`/`main`）；`X-Agent-Purpose` 入站优先（官方同链路也用 `conversation_topic`）；专家模式头 `X-Expert-Id` / `X-Expert-Team-Task` 透传（客户端没给不伪造）；**思考请求**补 `verbosity: "high"`（与官方 `modelSettings.text.verbosity` 同门控：与 `reasoning_summary: auto` 成对出现，非思考请求零改动） | 生效 |
| 22 | **上游失败码与会话身份入观测** | 归档新增 `upstream_kind` / `upstream_code`（如 `model_queued` / `6020`、`model_blocked` / `11102`）与 `conversation_derived`；`dropped` 标记带块类型（`messages[1].content[0]:thinking`）。此前「上游到底回了什么码」「被丢的是不是 thinking 块」在归档里查不到，队列类问题的频率完全无法离线统计 | 生效 |

第 17 条的判定与边界（细节以 `internal/upstream/dsml.go` 文件头为准）：

- **收尾语义**：还原当场发生（块闭合即转，不等到收尾帧），与上游流式行为一致；本层**从不伪造收尾**——
  仅在本 choice 已还原过调用且上游收尾为 `stop` 时改写为 `tool_calls`，`length` / `content_filter` 等原样保留。
- **截断边界**：若标记块在截断前已完整闭合，则已还原成 calls，而收尾仍是 `length`。此时各入口按自己
  已有的-致性规则处理：原生 Chat 透传（客户端自判）、Responses 标 `incomplete`、Anthropic 桥接**显式报错**
  （`unfinished or inconsistently terminated upstream tool calls`）。即“宁可大声失败，不把截断伪装成成功”。
- **通道开销**：弱判定常开（名单缺失是泄漏常态），但无标记的响应只做逐帧扫描、**零每帧分配**
  （`expand` 复用缓冲，模板只读引用）；由 `dsml_bench_test.go` 守护。
- **观测可区分**：`Converted/Seen/Rejected` 三计数分别回答“还原了几次”“识别到几块”“识别到但判定不过”，
  后者打 WARN 日志——排障时能一眼区分“没识别到”与“识别到但拒绝”。

---

## 2. 成长任务自动化策略（`growth.autotasks`）

判据实现仍是代码，启用集合/参数可配置（`internal/panel/autotask_policy.go`）：

| 配置键 | 语义 | 缺省 |
| --- | --- | --- |
| `disabled` | 屏蔽的任务码黑名单（与 `only` 同时命中时黑名单优先） | 空 = 不屏蔽 |
| `only` | 白名单；非空时只有列出的任务参与自动化 | 空 = 不启用白名单 |
| `order` | 执行顺序覆盖（队列与一键完成） | 空 = 按内置序 |
| `mp_codes` | 小程序口径专属任务码：裸码 = 整体覆盖内置表，前缀 `+` = 追加 | 空 = 内置表 |
| `allow_unknown_claim` | 对无判据实现的任务只做 accept + 达标领奖（绝不伪造事件） | `false` |
| `tasks.<code>.gap` | 判据事件间隔（Go 时长，如 `45s`） | 空 = 内置 |
| `tasks.<code>.target` | 进度目标覆盖（`0` = 以上游下发为准） | `0` |
| `tasks.<code>.attempt` | 「尝试型」展示标记覆盖（不改变执行逻辑） | 内置 |
| `tasks.<code>.window` | 计数窗口 `HH:MM-HH:MM`（支持跨零点；面板动作与 blackcat 排程共用） | 内置 `23:00-08:00` |
| `tasks.<code>.activity_id` | 上报事件的 activityId | 内置 `school_open_day_2026` |

- 判据事件的字段形状与埋点指纹必须作为 Go 代码存在（`internal/upstream/school.go` 等），配置只能引用已实现的动作；新增一类判据仍需改代码。
- `disabled` / `only` 只影响自动化动作集合，不改任务列表展示、不拦手动接口；被屏蔽任务「一键完成」返回 501。
- 全部字段热生效；mp 专属码优先运行时探测（双口径列表差集），静态表与配置只作兜底。
- 面板保存是深合并：`tasks` map 无法通过面板删除键，只能手工编辑 `config.json`。
- 内置动作码清单由后端 `GET /panel/api/tasks/actions` 下发。

---

## 3. 回主线禁忌

**不要恢复已删除的模块**：`upstream/payload.go`、`upstream/thinking.go`、`upstream/sanitize.go`、`upstream/tool_pairing.go`、`server/degrade.go` 及其测试——职责已由 `internal/forwarding/`、`internal/upstream/sse.go`、`internal/prompt/`、`internal/scrub/` 取代。

同理不要带回写死下限的降级改写（如上游 GPT `max_tokens` 抬升）。

跨协议兼容不引入模型名猜测式补丁：不按模型名增删参数、不搬移 system、不改写 Schema、不伪造 reasoning / 工具结果、不将截断转为成功。

> 例外：原生工具调用标记还原（§1 表第 17 条、dsml.go）不是「伪造工具结果」：它只把**模型自己发出的**
> 原生标记语法还原成结构化 `tool_calls`，不凭空造内容、不改请求参数。它是§15 式显式可见的近似
> 变换（归档 `rewritten` 可见 + 日志），配合生产真实泄漏正文夹具（testdata/leaked_markup.txt）与回归测试
> 满足「厂商直连补丁须提供真实上游脉敏请求 / 原始帧依据」的门槛。「绝不吞字节」为硬线：拿不准一律原文透出。

厂商直连补丁不等于 WorkBuddy 能力；采用补丁须提供真实上游脱敏请求 / 原始帧依据与回归测试。

---

## 4. 挂载点（冲突最可能）

| 能力 | 主要文件 |
| --- | --- |
| 转发契约 | **新增** `internal/forwarding/`、`internal/jsondoc/` |
| 响应管线 | `internal/upstream/{sse,tool_names}.go`、`internal/upstream/idle.go` |
| Responses / Messages | **新增** `internal/protocol/`、`internal/server/protocol.go`；`internal/server/{handler,logging}.go`；`upstream.ConsumeCompletion` 共享消费接口 |
| 图片形状与校验 | **新增** `internal/media/`、`internal/protocol/content.go`；`internal/forwarding/request.go`（Parse 归一）、`internal/server/handler.go`（hasImage 判定） |
| 工具结果图片策略 | **新增** `internal/media/{policy,hoist}.go`；`internal/protocol/request.go`（Decode 按入口策略）、`internal/server/protocol.go`（Mutated 重序列化）、`cmd/server/{config,main}.go` |
| 图片转码/压缩 | **新增** `internal/media/image.go`；`cmd/server/{config,main}.go`；面板表单项（`frontend/src/{configSchema.ts,views/ConfigView.tsx}`） |
| 重试与账号策略 | `internal/server/handler.go`、`internal/upstream/client.go` |
| 系统提示词 | `internal/prompt/`、`internal/config/prompt.go` + `internal/config/prompt_preview.go`（重新导出：`make templates-export`） |
| 指纹改写 | **新增** `internal/scrub/`；`internal/config/{schema,normalize,catalog}.go`、`cmd/server/main.go`、`internal/panel/panel.go`、面板表单项（`frontend/src/{configSchema.ts,types.ts,views/ConfigView.tsx}`） |
| 分域身份与 UA | **新增** `internal/upstream/identity.go`、`internal/auth/snapshot.go`；`internal/upstream/{headers,client,desktop,hint,usage}.go` |
| 缓存键 / 配置容错 | `internal/upstream/cache_key.go`、`cmd/server/{config,config_warnings,main}.go` |
| 出站代理 | `internal/proxy/`、`internal/auth/auth.go`、`internal/upstream/proxy.go` |
| 裸模型名默认域 | `internal/server/resolve_model.go` |
| 请求归档 / 被丢弃字段 | `internal/reqlog/reqlog.go`（`Event.dropped`）、`internal/server/logging.go`；面板 `frontend/src/views/LogsView.tsx` |
| 模型目录门禁 | **新增** `internal/upstream/catalog.go`、`internal/server/modelcheck.go`；`internal/server/handler.go`、`internal/panel/panel.go`、`cmd/server/main.go` |
| 原生工具调用标记还原 | **新增** `internal/upstream/dsml.go`（还原内核 + `markupStream` 帧展开 + `WithMarkupRepair` / `ToolNameAllowlist*`）；`internal/upstream/sse.go`（`consume` 内按 `o.markup` 展开帧，流式/非流式/整包快照三路径共用）；`internal/server/handler.go`（名单提取 + 挂载还原器 + `noteRewrite` 观测）；归档复用 `reqlog.Event.rewritten` + 面板 `LogsView.tsx`（无前端改动，复用改写展示通道） |
| 面板 | 源码 `frontend/src/`（React + Vite）；构建产物 `internal/panel/web/`（`go:embed`）；后端 `internal/panel/{config.go,panel.go,web_embed.go}` |
| 成长任务自动化策略 | `internal/config/{schema,normalize,catalog}.go`、`internal/panel/autotask_policy.go`（**新增**）、`internal/panel/{autotask,taskcenter,tasks}.go`、`internal/upstream/blackcat.go`（`InWindow`）、`internal/scheduler/{scheduler.go,blackcat.go}`、`cmd/server/main.go`、面板表单项（`frontend/src/{configSchema.ts,views/ConfigView.tsx}`） |
| 模型默认思考档覆盖 | **新增** `internal/upstream/effort_override.go`；`internal/upstream/{effort_catalog,model_catalog}.go`（EffortListing/ClampEffortWithDefault/RequestEffortDefault）、`internal/server/handler.go`（attempt 内的注入与回退目标 + 归档 `rewritten`）、`internal/config/{schema,normalize,catalog}.go`、`cmd/server/{main.go,apply_contract_test.go}`、面板表单项（`frontend/src/{configSchema.ts,views/ConfigView.tsx}`） |

---

## 5. 出站指纹改写策略（`fingerprint_*`）

内置指纹表（`internal/scrub/`）默认全启用，可按稳定 ID 停用或覆盖替换文本；自定义规则叠加在最上层。
全部字段热生效（面板保存即生效，无需重启）。

| 配置键 | 语义 | 缺省 |
| --- | --- | --- |
| `fingerprint_rewrite` | 总开关（关 = 严格逐字透传，内置与自定义一并忽略） | `false` |
| `fingerprint_disabled` | 禁用的内置规则 ID 黑名单（未知 ID 告警并按配置剔除；停用 `err-11128` 会额外告警） | 空 = 全启用 |
| `fingerprint_overrides` | 逐内置规则的替换文本覆盖（键 = rule_id；空串 = 用内置默认） | 空 = 全用内置默认 |
| `fingerprint_rules` | 自定义规则（在内置表之上叠加，语法见面板「规则文本」提示） | 空 = 无自定义 |

内置规则 ID 清单（面板「管理内置指纹表」弹窗可视化编辑）：

| ID | 类型 | 说明 |
| --- | --- | --- |
| `hdr-billing` | 模式 | 图灵/计费 header 键值段整段删除（大小写不敏感） |
| `hdr-billing-bare` | 模式 | 残留裸键名缩写（默认 `x-anthropic-billing-hdr`，可覆盖） |
| `cc-kv` | 模式 | 尾随 `cc_` 键值段整段删除 |
| `identity-claude-code` | 逐字 | Claude Code CLI 身份句（official CLI → CLI tool） |
| `identity-claude-agent-sdk` | 逐字 | Claude Agent SDK 身份句（Agent SDK → Agent SDK tool） |
| `pr-branch` | 逐字 | `Main branch (…)` → `Default branch (…)` |
| `identity-codex` | 逐字 | Codex CLI 身份句 → Codex CLI tool |
| `identity-codex-alt` | 逐字 | `You are Codex,` → `You are the Codex assistant,` |
| `feedback-issue` | 逐字 | 反馈 issue 句（give → provide feedback） |
| `err-11128` | 逐字 | 裸 `11128` → `11-128`（上游反探测错误码） |

遍历通道：`messages[].content`（字符串与多模态 text 块）、`reasoning_content` / `reasoning`、
`tool_calls[].function.arguments`，以及 `tools[].function.description` 与 `parameters.properties.*.description` / `.title`。
image part 与工具名不改（改写会破坏数据或契约）。

---

## 6. 模型默认思考档覆盖（`upstream.effort_default_overrides`）

上游模型目录下发 `reasoning.defaultEffort`，本仓还有一份产品级静态兜底默认档（`internal/upstream/effort_catalog.go`）——
两者都是**上游/编译期**口径，用户改不了；而「默认档」正是客户端不指定档位（或指定了一个该模型不支持的档位）时
实际执行的档位。本表是这份口径的唯一本地真相：**本地的覆盖上游配置**，空表 = 完全沿用上游（零行为变化）。

| 配置键 | 语义 | 缺省 |
| --- | --- | --- |
| `upstream.effort_default_overrides` | 模型默认思考档覆盖表（键 = 模型名，值 = 档位名） | 空 = 沿用上游 |

键位写法（大小写与首尾空白不敏感；未知域前缀告警并按配置剔除）：

| 键 | 作用范围 |
| --- | --- |
| `model` | 两个域都生效 |
| `cn:model` / `global:model` | 只该域生效（同一模型名在两个域的合法档位可以不同，见 §1 第 1 条） |
| `*` / `cn:*` / `global:*` | 通配，只作用于**档位能力已知**的模型（不替上游编造能力） |

取值：`minimal` / `low` / `medium` / `high` / `xhigh` / `max`。`off` / `none` 是「关闭思考」的开关值而不是档位，
一律拒绝（本地默认档不允许静默打开或关掉思考）；形态非法的取值（含空白/非标识符）在启动/保存时 fail fast，
形态合法但不在已知档位表内的值只告警（档位名集合由上游定义，本仓不把它变成封闭枚举）；
上游支持集已声明的新档位同样可用于默认注入和越界回退，不要求存在于静态排序表。

生效面（三处共用同一份表，保证「宣告的档位」与「可执行的档位」永不漂移）：

1. **宣告**：`/v1/models` 与面板「模型能力」页的 `reasoning_default_effort`（客户端照宣告选档是硬前提）；
2. **注入**：客户端三种档位别名都没带、也没用 `thinking.type=disabled` 显式关思考时，出站补上该默认档；
3. **越界回退**：请求档位合法但该模型不支持时，回退目标是本地默认档（未配置时保持既有「≤请求档位的最高支持档」规则）。

边界与不可协商项：

- 覆盖值先落到该模型的支持集上（命中即按**声明形态**回写，未命中按档位序钳位）——绝不宣告或发出一个必被上游 400 的档位；
- 逐模型显式条目在档位能力未知时也生效（那是用户对具体模型能力的声明）；通配则只在能力已知时生效；
- `off` / `none` 请求、`thinking.type=disabled` 请求一律不注入（把用户关掉的思考又打开比 400 更坏，同 §1 第 1 条）；
- 注入与越界回退都是**显式可见**的近似变换：归档 `rewritten` 记为 `reasoning.effort:默认→<档位>` /
  `reasoning.effort:<原值>→<出站值>`，Responses 桥在响应体 `reasoning.effort` 回显实际生效值；
  跨域重试时从原始入参重新构造回显，不沿用失败尝试的档位；
- 归一位点在 handler 的 attempt 循环内（按**实际选中账号**的域解析），因为多号重试可能跨域；
- **整表替换**（与 `fingerprint_rules` 这类 slice 字段同语义）：提交里出现该键 = 整张表被它替换
  （删键、清空都生效）；不提交该键则保持磁盘现值。map 字段在 `encoding/json` 里默认是逐键**合并**，
  所以配置解析层对这一项显式清空基准（`config.decodeInto`），所有调用方直接使用整表替换语义；
  同次提交中大小写/首尾空白归一后重名的键直接报错，不随机挑选取值。§2 里
  `growth.autotasks.tasks` 的「无法通过面板删键」是同一根因的另一面，未一并改动；
- 全部热生效（进程级原子快照整体替换；面板「模型档位」页可视化编辑），无需重启。

---

## 7. 会话身份、上游失败码与缓存对齐（§1 第 19–22 条）

### 7.1 背景：官方客户端靠什么拿到前缀缓存命中

对 `WorkBuddy-win32-x64-user-5.6.2.39458645-35219ed6.exe`（Global）与 5.7.6（CN）两个安装包的
完整逆向（`resources/app.asar` + `app.asar.unpacked/cli/dist/**`）结论：

- **没有**任何针对具体模型的缓存开关：不发 `cache_control`、不发 `prompt_cache_key`；
  唯一会写 `cache_control` 的规则（`CacheControlFormatRule`）挂在兼容处理层，而该层对
  **官方网关域名直接 `return`**（`isProductGatewayHost`）——即官方产品路径一个缓存标记都不发。
- 官方的高命中来自三条纪律，而不是机制：
  1. **会话身份恒定**：`X-Conversation-ID`（会话内不变）+ `X-Conversation-Request-ID`（一次 user send 内
     所有 tool 轮/重试复用同一值）+ `acp-connection-id`；
  2. **前缀逐字不变**：系统提示词与工具集稳定；工具按 `promptCacheStability` 排序
     （`orderToolsForPromptCache`：stable → 稳定 MCP 工具 → volatile → 其余），易变工具（ToolSearch/
     Agent/Skill/Repl…）永远排在最后，避免增删时错位整块前缀；历史消息逐字回放（含 `reasoning`）；
  3. **账号/落点不漂移**：连接级 failover 只换 origin 重放（`NetworkFailover` + `applyPinnedAddress`），
     不换账号。
- 与「glm-5.3 缓存极低」最相关的两条差异因此是：**会话身份缺失**（第三方客户端不带、网关不伪造）与
  **思考历史可选回放**（glm-5.3 是 `onlyReasoning` 纯思考模型，思考体量最大）。
- 官方还有一处 glm 专属配置（`product.json` 的 `fillToolCallContentModelWhitelist: ["glm","claude"]`），
  但两个安装包里**没有任何 JS 消费它**（是死配置），且与缓存无关。

### 7.2 本仓落地了什么（哪些默认生效、哪些默认关）

| 项 | 默认 | 边界 |
| --- | --- | --- |
| 容量队列 429 + Retry-After + hint | 生效 | 只认结构化 `code`/`bizCode`（6020/6021/6022）与 HTTP 202；不猜文案（6022 文案常含 "limit"，会被限流词表误伤）。账号零惩罚、不轮转；`data.retry_after` <1000 视为秒、≥1000 视为毫秒，夹在 [1s, 2h] 之外丢弃 |
| `X-Agent-Type` 恒发 / `X-Agent-Purpose` 透传 / 专家头透传 | 生效 | 入站值优先；缺省派生规则与官方 `resolveAgentType()` 一致（有 parent = subagent，否则 main）；专家头客户端没给就不发 |
| 思考请求补 `verbosity: "high"` | 生效 | 门控与官方一致：只在 `reasoning_effort`/`reasoning.effort` 非空或 `thinking.type=enabled` 且客户端自带值缺失时注入（与 `reasoning_summary: auto` 成对）；`off`/`none`/`disabled` 不注入 |
| 归档 `upstream_kind`/`upstream_code`/`conversation_derived`、`dropped` 带块类型 | 生效 | 只记码不记正文；`dropped` 上限 8 条不变（类型后缀不增加条目数） |
| `upstream.derive_conversation_id` | **`false`** | 只在「客户端没给会话 id + 网关算出稳定会话键」时派生；派生值只进请求头/缓存键材料，**不返回客户端**，也不污染入站语义（入站仍不伪造）。开启后 A/B 观察到收益再改默认 |

未做（有意留下，避免一次改动里混入语义变更）：

- **思考回放**：Anthropic 桥接仍**丢弃** `thinking`/`redacted_thinking`（只是归档标记带上类型以便量化频率）。
  把 thinking 映射到 assistant 的 `reasoning` 会改变提示词语义（回答风格/长度可能变化），
  需先用 §7.3 的频率数据 + 小流量质量抽检验证。
- 逐消息元数据注入（`conversationRequestId`/`messageId`/`entryId`/`model`/`queuePosition`…）、
  连接级 origin failover、`Accept-Encoding: br`。

### 7.3 怎么判断这些改动值不值（用本仓已有的仪表）

1. **先量化队列与丢弃**（改动前无法离线统计，现已入档）：
   `request-logs` 里筛 `upstream_code=6020/6021/6022` 与 `dropped` 含 `:thinking` 的记录数——
   这两个频率直接决定第 19 条与「思考回放」的优先级。
2. **会话身份 A/B**：`upstream.derive_conversation_id` 开关前后同模型对比
   「缓存命中率 × 积分/1M Token」（面板「用量」两列同表）；同时看 `conversation_derived` 计数确认生效。
3. **把折扣算成钱**：逐请求样本回归 `credit ≈ a·miss_tokens + b·hit_tokens + c·completion_tokens`，
   `a/b` 即上游实际给的缓存折扣；再按
   `月省 ≈ 月 token × 面板「积分/1M Token」× (1 − 1/折扣) × 命中率提升` 折算。
4. **回滚口径**：第 20 条一个布尔开关即可回退；第 19/21/22 条是纯增量（不改既有成功路径的字节）。

---

## 附：同步记录

| 日期 | 并入范围 | 内容 |
| --- | --- | --- |
| 2026-10-08 | `e05edb7..d66384d`（5 个实质提交） | 企业版账号能力门控 + 企业额度分流（`get-enterprise-user-usage`）；暂停选号单列统计；到期提醒按到期时间排序；图表 tooltip 归位 `<g>`；夜猫子门控测试时钟钉住；版本号 `1.13.0-panel`。已同时移植上游 5 个测试文件与 2 个前端用例 |
| 2026-10-11 | 上游 `7595a0a`（能力吸收，非整提交合入） | **原生工具调用标记还原（DSML→tool_calls）**：先在 fork 上用上游生产夹具复现「未声明 tools 时标记泄漏成正文」，确认是 fork 真实能力缺口；按 fork 自身 `StreamOption`/`ConsumeCompletion` 重写还原层（非移植上游 handler 接线），流式/非流式/整包快照共用 `consume` 内帧展开；落地姿态从上游「静默改写」换成 §15 式「显式可见」（归档 `rewritten` + 日志）。上游同批的 `8391a05`（面板按需拉取）不合入：改的是 fork 不存在的 `internal/panel/app.js`，且诉求已被 fork 的 `ExpiryCard` 缓存方案覆盖 |
| 2026-10-14 | 本仓（客户端逆向对齐，§7） | **容量队列 6020/6021/6022 + HTTP 202** 从 502/通用 4xx 改为 429 + Retry-After + 带量化信息的 `gateway_hint`（账号零惩罚、不轮转）；**派生会话身份** `upstream.derive_conversation_id`（默认关，A/B 用）；`X-Agent-Type` 恒发 + `X-Agent-Purpose`/专家头透传 + 思考请求补 `verbosity=high`；归档新增 `upstream_kind`/`upstream_code`/`conversation_derived`，`dropped` 标记带块类型。依据：5.6.2/5.7.6 官方客户端 bundle 逆向（官方产品路径不发任何缓存标记，高命中来自会话身份恒定 + 前缀逐字不变 + 不换号） |

> 同步方式：fork 与上游无共同祖先，采用「按文件取上游版本 + 手工 port」，不用 rebase / cherry-pick。
> 更早的基线 `e05edb7` 为 fork 首次记录差异时的比对点；其后上游提交均已按上表并入。
