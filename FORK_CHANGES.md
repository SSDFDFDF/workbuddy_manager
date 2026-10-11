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
| 1 | **转发契约（严格、不改写）** | 出站只做校验与编码：未知字段与大整数完整保留，无法表达的输入直接 400（跨协议入口的提示/状态字段另有「接受即丢」，见第 15 条）；删除系统提示词清洗、内容拦截降级重试、思考自动补档、GPT `max_tokens` 抬升、工具历史重排。**例外**：越界 `reasoning_effort` 按实际选中账号所属域的模型档位能力归一（≤请求档位的最高支持档），改写进归档 `dropped` 列可见 | 生效（无开关） |
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

## 附：同步记录

| 日期 | 并入范围 | 内容 |
| --- | --- | --- |
| 2026-10-08 | `e05edb7..d66384d`（5 个实质提交） | 企业版账号能力门控 + 企业额度分流（`get-enterprise-user-usage`）；暂停选号单列统计；到期提醒按到期时间排序；图表 tooltip 归位 `<g>`；夜猫子门控测试时钟钉住；版本号 `1.13.0-panel`。已同时移植上游 5 个测试文件与 2 个前端用例 |
| 2026-10-11 | 上游 `7595a0a`（能力吸收，非整提交合入） | **原生工具调用标记还原（DSML→tool_calls）**：先在 fork 上用上游生产夹具复现「未声明 tools 时标记泄漏成正文」，确认是 fork 真实能力缺口；按 fork 自身 `StreamOption`/`ConsumeCompletion` 重写还原层（非移植上游 handler 接线），流式/非流式/整包快照共用 `consume` 内帧展开；落地姿态从上游「静默改写」换成 §15 式「显式可见」（归档 `rewritten` + 日志）。上游同批的 `8391a05`（面板按需拉取）不合入：改的是 fork 不存在的 `internal/panel/app.js`，且诉求已被 fork 的 `ExpiryCard` 缓存方案覆盖 |

> 同步方式：fork 与上游无共同祖先，采用「按文件取上游版本 + 手工 port」，不用 rebase / cherry-pick。
> 更早的基线 `e05edb7` 为 fork 首次记录差异时的比对点；其后上游提交均已按上表并入。
