// main.go workbuddy_manager 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/config"
	"workbuddy_manager/internal/config/runtime"
	"workbuddy_manager/internal/media"
	"workbuddy_manager/internal/panel"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/prompt"
	"workbuddy_manager/internal/redisstore"
	"workbuddy_manager/internal/reqlog"
	"workbuddy_manager/internal/scheduler"
	"workbuddy_manager/internal/scrub"
	"workbuddy_manager/internal/server"
	"workbuddy_manager/internal/session"
	"workbuddy_manager/internal/upstream"
	"workbuddy_manager/internal/usage"
)

// appVersion 网关版本。
const appVersion = "1.0.0-new"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := config.WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = config.Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = config.Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	config.LogWarnings(cfg)

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)

	// 域策略解析器：裸模型名按 config model_default_realm 决定 cn/global/auto 系列。
	// handler（chat 路由）与粘性闭包必须共用同一实例，否则二者域不一致；实例常驻，
	// 配置热改走 SetDefault（热路径读原子值）。
	realmResolver := server.NewRealmResolver(cfg.ModelDefaultRealm, func(realm string) bool {
		return len(p.AvailableUIDsForRealm(realm)) > 0
	})
	realmResolver.RealmReadyForModel = func(realm, model string) bool {
		return len(p.WeightedAvailableUIDsForModelRealm(model, realm)) > 0
	}
	if cfg.ModelDefaultRealm != "cn" {
		log.Printf("[realm] 裸模型名默认域=%s（显式 cn:/global: 前缀恒优先）", cfg.ModelDefaultRealm)
	}
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}

	// 会话粘性路由：**常驻构建**（config 可热改启停 / TTL / GC 周期）。
	// Reconfigure 按配置初始化：启用时才从 Redis 恢复绑定并跑 GC；禁用时
	// 零分配零 GC（Resolve/Bind 内部按 enabled 短路）。
	sessRouter := session.New(session.Config{
		TTL:        cfg.SessionTTL,
		GCInterval: cfg.SessionGCInterval,
		Store:      store,
		Available:  p.AvailableUIDs,
		// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
		// 裸名按 model_default_realm 策略（与 handler 同一 resolver）。
		AvailableForModel: realmAwareAvailableForModel(p, realmResolver),
	})
	sessRouter.ReconfigureFingerprint(cfg.SessionTTL, cfg.SessionGCInterval, cfg.SessionSticky.Enabled, cfg.SessionFingerprint)
	defer sessRouter.StopGC()
	sessCount := func() int { return sessRouter.Count() }

	up := upstream.New()
	upstream.StartVersionCheckAsync()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// 上游出站配置热改快照（profiles/域名/开关/超时，见 upstream/options.go）。
	// 短 RPC 超时不再写 HTTP.Timeout：由 ShortClient() 按快照派生，避免运行时
	// 改写共享 client 字段的数据竞争。
	up.Configure(upstreamOptions(cfg))
	// 聊天 SSE 首字节前（响应头）上限：写进底层 Transport（重建连接池 + 客户端指针
	// 原子替换，见 upstream.SetHeaderTimeout）；配置保存路径同样调用（热项）。
	up.SetHeaderTimeout(time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second)
	up.CacheSecret, err = upstream.LoadCacheSecret(stateSibling(cfg.StateFile, "cache-secret"))
	if err != nil {
		log.Fatalf("cache secret: %v", err)
	}
	// 出站指纹改写层（默认关闭 = 严格逐字透传）。自定义规则非法时
	// normalize 已 fail fast，这里构建不会失败。
	scrubLayer, err := buildScrubLayer(cfg.FingerprintRewrite, cfg.FingerprintDisabled, cfg.FingerprintOverrides, cfg.FingerprintRules)
	if err != nil {
		log.Fatalf("fingerprint rules: %v", err)
	}
	up.Fingerprints.Store(scrubLayer)
	// 工具结果图片策略（media.tool_images）：进程级原子快照，请求路径零锁读取。
	// ParseConfig 已校验合法性，这里失败属装配期异常。
	if err := media.SetToolPolicy(media.ToolPolicy(cfg.Media.ToolImages)); err != nil {
		log.Fatalf("media.tool_images: %v", err)
	}
	// 图片转码/压缩策略（media.image_transcode / image_max_dimension）：默认全关，
	// 关闭时请求路径不解码像素（只有形状/大小校验）。
	if err := media.SetImagePolicy(media.ImagePolicy{Transcode: cfg.Media.ImageTranscode, MaxDimension: cfg.Media.ImageMaxDimension}); err != nil {
		log.Fatalf("media image policy: %v", err)
	}
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// 出站代理（普通正向代理或 Resin）：proxy_url / resin_url 非空才接入。
	// 转发层（proxy.Dynamic）自 upstream.New 起常驻，这里只设置当前实例；
	// 之后配置热改同样只换指针，不触碰底层 Transport/连接池。未接入 = nil 直连。
	up.SetProxy(cfg.ProxyClient)
	if cfg.ProxyClient != nil {
		log.Printf("[proxy] 出站代理已接入：mode=%s platform=%s auth=%s",
			cfg.ProxyClient.Mode(), cfg.ProxyClient.Platform(), cfg.ProxyClient.AuthVersion())
	}
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))
	// 模型默认思考档的本地覆盖接线（upstream.effort_default_overrides）：进程级快照，
	// 空表 = 完全沿用上游；保存配置时同函数热应用（见 saveConfigTx）。
	upstream.SetEffortDefaultOverrides(cfg.Upstream.EffortDefaultOverrides)

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 夜猫子计数窗口覆盖（growth.autotasks.tasks.black_cat.window）：
		// 与面板侧动作同一份配置，nil = 内置 23:00–08:00。
		BlackcatWindow: blackcatWindowFrom(cfg),
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	switch {
	case !cfg.Schedule.TokenRefreshEnabled:
		log.Printf("token 到期前兜底刷新已禁用（schedule.token_refresh_enabled=false）")
	case cfg.TokenRefreshInterval > 0:
		log.Printf("token 到期前兜底刷新：每 %s（剩余 <%s 即续期，覆盖无流量账号）",
			cfg.TokenRefreshInterval, scheduler.TokenRefreshWindow)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := runtime.New(runtime.Snapshot{
		APIKey:           cfg.APIKey,
		SoftCooldown:     cfg.SoftRateDur,
		RecordClientInfo: cfg.Logging.RequestClientInfo,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	// 归档目录由 state_file 派生（state_file 是重启项，故这里固定装配期路径；
	// 热改归档参数不会跟着状态文件搬家）。
	requestLog := reqlog.New(archiveConfigFrom(cfg.StateFile, cfg))
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	// hot 配置保存路径的热应用目标集合（见 saveConfigTx 与 hotTargets 定义）。
	// handler/panel 在下面创建后回填：config API 的 SaveConfig 闭包引用同一对象，
	// 因此闭包创建顺序不再依赖组件创建顺序。
	hot := &hotTargets{
		live:       live,
		pool:       p,
		upstream:   up,
		schedule:   sch,
		requestLog: requestLog,
		prompt:     prompt.NewHolder(cfg.PromptRules),
		session:    sessRouter,
		realm:      realmResolver,
		stateFile:  cfg.StateFile,
	}

	pn := panel.New(panel.Config{
		Pool:        p,
		Usage:       rec,
		RequestLog:  requestLog,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     appVersion,
		Live:        live,
		// 出站代理（nil = 未接入）：面板 OAuth 登录阶段用临时身份走代理，
		// 登录成功后（仅 Resin）inherit-lease 给 UID。
		Proxy: cfg.ProxyClient,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile: stateSibling(cfg.StateFile, "output_probes.json"),
		// 配置域接口：读写逻辑与字段语义都在 internal/config，面板只挂载。
		ConfigAPI: config.NewAPI(config.APIConfig{
			ConfigPath: *cfgPath,
			LoadConfig: func() (any, error) {
				c, err := config.Load(*cfgPath)
				if err != nil {
					return nil, err
				}
				// 面板配置页据此展示 `_warnings`（未知键），不静默吞掉配置里
				// 有键名拼错这件事。
				config.LogWarnings(c)
				return c, nil
			},
			SaveConfig: func(raw []byte) ([]string, error) {
				return saveConfig(raw, *cfgPath, hot)
			},
			// 配置页"立即预览"：解析草稿 prompt 段并返回各域生效正文，不落盘。
			PreviewPrompt: func(raw []byte) (any, error) {
				return config.PreviewPromptConfig(raw)
			},
			VersionInfo: func() any { return upstream.GetLatestVersionInfo() },
		}),
	})
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:        p,
		Upstream:    up,
		APIKey:      cfg.APIKey,
		Session:     sessRouter,
		StickyCount: sessCount,
		RedisMode:   redisMode,
		// Redis 镜像队列丢弃计数：/status 透出，面板/脚本据此判断镜像是否跟上。
		RedisDroppedWrites: func() uint64 {
			if r, ok := store.(interface{ DroppedWrites() uint64 }); ok {
				return r.DroppedWrites()
			}
			return 0
		},
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		RequestLog:   requestLog,
		// 分域提示词规则（cn/global 各一份，键 "" 为默认）。热改走 PromptHold；
		// 保存配置时由 saveConfigTx 整体 Store 新规则。
		PromptHold: hot.prompt,
		// 来源记录开关经 config/runtime 快照热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		// 裸名默认域解析器（cn/global/auto），与粘性闭包共用同一实例。
		RealmResolver: realmResolver,
		// 服务级入站准入（读取 + 解析 + 图片校验的并发/字节预算），见 server/ingress.go。
		MaxInflightRequests: cfg.Server.MaxInflightRequests,
		MaxInflightBytesMB:  cfg.Server.MaxInflightBytesMB,
		IngressWait:         cfg.IngressWaitDur,
	})
	// 回填延迟绑定目标（在此之前 HTTP 尚未开始服务，无并发写入）：面板保存配置
	// 时经这两个句柄热改入站准入/会话粘性以外的处理器侧字段。
	hot.handler = h
	hot.panel = pn
	// 成长任务自动化策略快照（growth.autotasks）：装配即建并注入，保存配置时整体替换。
	pn.SetAutotaskPolicy(panel.NewAutotaskPolicy(cfg))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 入站满载观测（仅在确实发生等待/拒绝时打印，常态零输出）。
	stopIngressLog := h.StartIngressLog(30 * time.Second)
	defer stopIngressLog()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	sch.StartTokenRefresh(ctx, cfg.TokenRefreshInterval)

	// 启动即预热模型目录快照 + 积分倍率表（同一次探测，见 warmModelCatalog）：
	//   - 目录快照是出站门禁与 /v1/models 的唯一事实来源，不预热则门禁一直
	//     fail-open（不报错，但也拦不住不存在的模型）；
	//   - 倍率表只在探测成功时填充，不预热则 ModelRate 恒返回空串——积分保底的
	//     目录兜底在空窗期内形同虚设，触底号会被当成「收费未知」放行并打穿
	//     （实测：重启后 2 分钟，97 分的账号打收费模型归零）。
	// 异步执行：不阻塞监听启动；失败仅记日志（面板「模型能力」页可手动刷新补上）。
	go warmModelCatalog(ctx, up, p)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	// 主流程必须等 Shutdown 真正完成才能返回：否则 main 的 defer（rec.Stop/
	// requestLog.Close/pool.Close/store.Close）会在在途请求还在写统计时执行，
	// 造成“服务已停但请求被记账到已关闭组件”的截断。
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	switch {
	case cfg.Server.MaxInflightRequests > 0 || cfg.Server.MaxInflightBytesMB > 0:
		log.Printf("入站准入：并发解析上限 %d 请求 / %d MiB（满载等待 %s，超时回 503 server_busy）",
			cfg.Server.MaxInflightRequests, cfg.Server.MaxInflightBytesMB, cfg.IngressWaitDur)
	default:
		log.Printf("入站准入已关闭（server.max_inflight_requests / max_inflight_bytes_mb 均为 0）")
	}
	log.Printf("workbuddy_manager listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	select {
	case err := <-serveErr:
		// 监听失败（端口占用/地址无效）：直接退出，不做停机流程。
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	case <-ctx.Done():
		// 收到信号先落盘一次（硬杀兜底：Shutdown 若被第二个信号打断，状态已尽力保全）；
		// 排空在途请求后的最终落盘由 main 的 defer p.Close() 负责。
		p.Flush()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		err := srv.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			log.Printf("WARN: 优雅停机未在 %s 内完成（在途请求将随进程退出被中断）: %v", shutdownGrace, err)
		} else {
			log.Printf("HTTP 已优雅停机：在途请求处理完毕")
		}
		<-serveErr // Shutdown 后 ListenAndServe 立即返回，确保监听 goroutine 已退出
	}
	log.Printf("bye")
}

// shutdownGrace 优雅停机的等待上限：给在途请求（含长 SSE）留出收尾时间，
// 超时则强断剩余连接并进入退出流程。取 30s 是折中：既不因个别长流无限期
// 拖住重启，也明显长于旧实现固定的 5s（大上下文请求常常来不及写完）。
const shutdownGrace = 30 * time.Second

// warmModelCatalog 启动预热：刷新各域模型目录快照 + 积分倍率表。
//
// 一次刷新同时产出两样东西（都只在探测成功时写入）：
//   - **模型目录快照**（upstream.Client.Catalog）：网关出站门禁与 /v1/models 的
//     唯一事实来源。刷新制而非懒加载：客户端请求路径上零上游探测，行为可预测；
//   - 积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么必须预热倍率：倍率表只在探测成功时填充，此前是懒触发（被 /v1/models 或
// 面板模型页访问才跑）。重启后到首次触发之间的空窗期里 ModelRate 恒返回空串，
// 保底的目录兜底判不出收费，触底号会被当成「收费未知」放行并打穿（实测：重启后
// 2 分钟，97 分的账号打收费模型归零）。目录快照同理会留出门禁 fail-open 空窗。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——面板「模型能力」页可手动刷新）；
// global 域仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelCatalog(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if infos, err := up.RefreshCatalog(a); err != nil {
				log.Printf("WARN: [upstream] warm model catalog (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model catalog: cn ok (%d models)", len(infos))
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	// 用 GlobalOn() 读热改快照（静态字段只作装配期回退）。
	if up.GlobalOn() && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				if infos, err := up.RefreshCatalog(a); err != nil {
					log.Printf("WARN: [upstream] warm model catalog (global): %v", err)
				} else {
					log.Printf("[upstream] warm model catalog: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：叠加 → 校验 → 整份覆盖落盘 → 热应用 → 返回需重启字段。
//
// 步骤：
//  1. 提交体做 JSON 合法性检查（非法直接拒绝，不碰磁盘）；
//  2. 读磁盘当前配置并按当前 schema 解析两份：差异基准 + 工作副本；
//  3. 把提交叠加到工作副本上（提交里没出现的键保持现值）并校验
//     （与启动同一套 Default+normalize），失败直接返回、不落盘；
//  4. 把**整份 Config** 原子落盘（config.WriteFileAtomic，含 Docker 单文件
//     bind mount 回落）：写出去的文件永远是完整当前结构，历史/未知键随之消失；
//  5. 热应用能立即生效的字段，并按「差异 ∩ 需重启目录」返回清单
//     （字段名单唯一真相：internal/config/catalog.go）。
//
// 串行化：整个「读 → 改 → 写」在 config.FileTx 事务锁里（见 internal/config/lock.go）。
// 多个保存请求（多标签页/脚本重试）共用同一份 config.json 与同一个 .tmp，无锁时
// 已实测出现 rename ENOENT，以及后写覆盖先写、磁盘版本与热生效顺序错位；
// 只给临时文件换随机名解决不了丢更新。同一把锁也被版本迁移的回写路径持有，
// 否则两者交错时"迁移回写"会覆盖掉一次刚保存的改动。
//
// 注意：锁不可重入，函数体内部不得再调用 config.FileTx / config.Load。
// hotTargets 配置保存路径的热应用目标集合。
//
// 为什么用结构体：热应用点随热化字段增多（鉴权快照/池/排程/上游/归档/提示词/
// 处理器/面板），全部塞进 saveConfig 形参会失控；且 handler 与 panel 在装配
// 后半段才创建（panel 需要 SaveConfig 闭包、handler 需要 panel 挂载），只能在
// 创建后回填——闭包与回填引用同一个对象。回填发生在 HTTP 开始服务之前，
// 保存请求期间只读，无数据竞争。
type hotTargets struct {
	live       *runtime.Holder
	pool       *pool.Pool
	upstream   *upstream.Client
	schedule   *scheduler.Scheduler
	requestLog *reqlog.Recorder
	prompt     *prompt.Holder
	session    *session.Router
	realm      *server.RealmResolver

	// stateFile 装配期状态文件路径（state_file 为重启项）：派生路径（归档目录等）
	// 在热应用时保持与原目录一致，不因同一次保存里改了 state_file 而分裂持久化视图。
	stateFile string

	handler *server.Handler // 延迟绑定
	panel   *panel.Panel    // 延迟绑定
}

func saveConfig(raw []byte, path string, hot *hotTargets) ([]string, error) {
	var (
		restart []string
		err     error
	)
	txErr := config.FileTx(func() error {
		restart, err = saveConfigTx(raw, path, hot)
		return err
	})
	if txErr != nil {
		return nil, txErr
	}
	return restart, nil
}

// loadForSave 读磁盘配置并解析**两份**对象：差异基准（old）与待改写的工作副本（base）。
//
// 为什么要两份：ParseConfigInto 是原地改写，工作副本被提交覆盖后就没法再算差异了
// （restart_required 依赖 old vs new 的叶子差异）。
//
// 读不到 / 解析不了时的口径：工作副本从 Default() 起算（能继续用提交内容覆盖），
// 差异基准返回 nil —— DiffConfig 把 nil 视为"全部变更"，重启清单退回保守的全量
// （宁可多报不可漏报）。
func loadForSave(path string) (old, base *config.Config) {
	base = config.Default()
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("config: 读当前配置失败，按默认值基准保存（%v）", err)
		return nil, base
	}
	parsed, err := config.ParseConfigInto(oldRaw, config.Default())
	if err != nil {
		log.Printf("config: 当前配置不可解析，按默认值基准保存（%v）", err)
		return nil, base
	}
	work, err := config.ParseConfigInto(oldRaw, config.Default())
	if err != nil {
		return nil, base
	}
	return parsed, work
}

// saveConfigTx 执行保存事务本体；调用方必须已持有 config.FileTx（不可重入）。
//
// 语义：**全量覆盖**。提交的 JSON 只决定它出现的那部分键，其余键保持磁盘现值；
// 写下去的是完整 Config 序列化结果（当前 schema）。因此：
//   - 提交里不认识的键（旧键/拼错的键）：忽略，不落盘——写出去的文件里没有；
//   - 磁盘上的未知/历史键：随这次全量覆盖自然消失，不需要单独的剪枝步骤；
//   - 部分提交（只改几个键的脚本）不会把其他键重置成默认值。
func saveConfigTx(raw []byte, path string, hot *hotTargets) ([]string, error) {
	// 1) 读磁盘当前配置，得到差异基准与待改写的工作副本。
	oldCfg, base := loadForSave(path)

	// 2) 把提交叠加到基准上（JSON 里没出现的键不动），再校验。
	// 非法 JSON / 非法取值都在这里被挡下，直接返回、不落盘。
	newCfg, err := config.ParseConfigInto(raw, base)
	if err != nil {
		return nil, fmt.Errorf("提交的配置无效: %w", err)
	}
	// 保存时也把告警打出：用户刚改完配置就能看到哪一项没生效。
	config.LogWarnings(newCfg)

	// 3) auth_dir 热重载预检（必须在落盘前完成，否则失败会留下“文件已改但未生效”）：
	// 路径已存在且不是目录 → 拒绝；目录不存在按空目录处理（面板登录会 MkdirAll）。
	authDirChanged := oldCfg == nil || oldCfg.AuthDir != newCfg.AuthDir
	var newAuths []*auth.Auth
	if authDirChanged {
		if st, serr := os.Stat(newCfg.AuthDir); serr == nil && !st.IsDir() {
			return nil, fmt.Errorf("auth_dir %q 已存在且不是目录", newCfg.AuthDir)
		}
		auths, aerr := auth.LoadDir(newCfg.AuthDir)
		if aerr != nil {
			return nil, fmt.Errorf("auth_dir: %w", aerr)
		}
		newAuths = auths
	}

	// 4) 落盘：写的是**整份 Config**（当前 schema），不是提交原文也不是磁盘原文。
	// 运行期元数据（告警）不落盘；磁盘上的未知/历史键就此被覆盖掉。
	newCfg.Warnings = nil
	out, err := config.MarshalConfig(newCfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	if err := config.WriteFileAtomic(path, out); err != nil {
		return nil, err
	}

	// 5) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	// 字段名单的唯一真相是 internal/config/catalog.go（见 restartRequiredFields）。
	hot.live.Store(runtime.Snapshot{
		APIKey:           newCfg.APIKey,
		SoftCooldown:     newCfg.SoftRateDur,
		RecordClientInfo: newCfg.Logging.RequestClientInfo,
	})
	p, up, sch := hot.pool, hot.upstream, hot.schedule
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	// 归档参数热生效：开关/保留天数/上限整体换 writer（旧 writer 排空后停写）。
	hot.requestLog.Reconfigure(archiveConfigFrom(hot.stateFile, newCfg))
	// 提示词规则热生效：mode/preset/file/text/profiles 已在 normalize 阶段解析成
	// PromptRules，这里整体替换快照（请求路径下一次查表即用新规则）。
	hot.prompt.Store(newCfg.PromptRules)
	// 入站准入限额热生效（当前在途请求继续持有名额，新限额只影响后续判定）。
	if hot.handler != nil {
		hot.handler.SetIngressLimits(newCfg.Server.MaxInflightRequests, newCfg.Server.MaxInflightBytesMB, newCfg.IngressWaitDur)
	}
	// 会话粘性热生效（启停 / TTL / GC 周期）。ReconfigureHot：Redis 恢复放后台，
	// 不让同步的 LoadBinds（上限 30s）拖住配置保存与配置锁。
	if hot.session != nil {
		hot.session.ReconfigureHotFP(newCfg.SessionTTL, newCfg.SessionGCInterval, newCfg.SessionSticky.Enabled, newCfg.SessionFingerprint)
	}
	// 上游身份/域名/开关/超时快照热生效（profiles / client_name / device_token /
	// chat_base* / global.enabled / 各档超时）。三处 global 闸门同一次切换：auth 侧
	// （auth.SetGlobalEnabled）、上游侧（快照 GlobalEnabled）、handler 侧。
	if hot.upstream != nil {
		hot.upstream.Configure(upstreamOptions(newCfg))
		// 首字节超时热生效：值变化时重建 Transport/客户端（连接池重置），
		// 值未变时空操作（不会白拆连接池）。
		hot.upstream.SetHeaderTimeout(time.Duration(newCfg.Upstream.HeaderTimeoutSeconds) * time.Second)
		// 出站代理热生效（proxy_url / resin_*；nil = 取消代理恢复直连）：
		// 只换当前转发层的内部指针，不改底层 Transport/连接池。
		hot.upstream.SetProxy(newCfg.ProxyClient)
	}
	// 面板登录链路的出站代理同步替换。
	if hot.panel != nil {
		hot.panel.SetProxy(newCfg.ProxyClient)
	}
	auth.SetGlobalEnabled(newCfg.Global.Enabled)
	if hot.handler != nil {
		hot.handler.SetGlobalEnabled(newCfg.Global.Enabled)
	}
	// 裸名默认域策略热生效（handler 与粘性闭包共享同一 resolver 实例）。
	if hot.realm != nil {
		hot.realm.SetDefault(newCfg.ModelDefaultRealm)
	}
	// auth_dir 热生效：重扫目录并把账号池对齐到新目录（新账号加入、文件已删除的
	// 账号从池中移除；状态保留），面板登录/导入的落盘目录同步切换。
	if authDirChanged {
		if hot.pool != nil {
			hot.pool.SyncToDir(newAuths)
		}
		if hot.panel != nil {
			hot.panel.SetAuthDir(newCfg.AuthDir)
		}
		log.Printf("config: auth_dir → %s（重载 %d 个账号，账号池已对齐）", newCfg.AuthDir, len(newAuths))
	}
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	// 指纹改写层热生效：开关与自定义规则都无需重启（整体替换不可变层）。
	newScrub, err := buildScrubLayer(newCfg.FingerprintRewrite, newCfg.FingerprintDisabled, newCfg.FingerprintOverrides, newCfg.FingerprintRules)
	if err != nil {
		return nil, err
	}
	up.Fingerprints.Store(newScrub)
	// 模型默认思考档的本地覆盖热生效（upstream.effort_default_overrides）：进程级快照
	// 整体替换（同 fingerprint 层的写法），下一次 /v1/models 与下一个请求即用新表。
	// 不判 upstream 是否为 nil：快照是包级单例，装配前后都要与配置一致。
	upstream.SetEffortDefaultOverrides(newCfg.Upstream.EffortDefaultOverrides)
	// 工具结果图片策略热生效：进程级原子快照，无需重启（normalize 已校验取值）。
	if err := media.SetToolPolicy(media.ToolPolicy(newCfg.Media.ToolImages)); err != nil {
		return nil, err
	}
	if err := media.SetImagePolicy(media.ImagePolicy{Transcode: newCfg.Media.ImageTranscode, MaxDimension: newCfg.Media.ImageMaxDimension}); err != nil {
		return nil, err
	}
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetTokenRefreshInterval(newCfg.TokenRefreshInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	// 夜猫子计数窗口热生效（growth.autotasks.tasks.black_cat.window）：与面板侧动作同源。
	sch.SetBlackcatWindow(blackcatWindowFrom(newCfg))
	// 成长任务自动化策略热生效（growth.autotasks）：整体替换面板侧快照，任务中心 /
	// 一键完成 / growth 排程的下一次动作即用新策略（无需重启）。
	if hot.panel != nil {
		hot.panel.SetAutotaskPolicy(panel.NewAutotaskPolicy(newCfg))
	}

	return restartRequiredFields(oldCfg, newCfg), nil
}

// restartRequiredFields 返回本次保存中「确实改动、且无法热生效」的字段名。
//
// 基准是 oldCfg（保存前的磁盘配置）与 newCfg（保存后的配置），两者都经
// Default+normalize 解析：没碰过的字段不会出现，因此"只改了个热字段"不会再回
// "20 项需重启进程生效"。oldCfg 为 nil（旧文件不可解析）时 DiffConfig 视为
// 全部变更，退回保守的全量清单——宁可多报不可漏报。
//
// 字段名单不在这里：唯一真相是 internal/config/catalog.go。本函数只做
// 「差异 ∩ Restart」的集合运算，所以新增/热化字段时只需改目录一处，
// TestApplyClaimsEveryHotField 会在"目录说 Hot 但没人热应用"时失败。
func restartRequiredFields(oldCfg, newCfg *config.Config) []string {
	cat := config.Entries()
	var out []string
	for _, p := range config.DiffConfig(oldCfg, newCfg).Slice() {
		if cat.IsRestartPath(p) {
			out = append(out, p)
		}
	}
	return out
}

// blackcatWindowFrom 由配置推导夜猫子计数窗口覆盖（nil = 内置 23:00–08:00）。
//
// 装配期与热应用共用同一口径，避免「重启后与热改后行为不一致」：窗口来自
// growth.autotasks.tasks.black_cat.window（与面板 runBlackCat 同一字段）。
func blackcatWindowFrom(cfg *config.Config) *scheduler.CountWindow {
	p, ok := cfg.Growth.Autotasks.Tasks["black_cat"]
	if !ok || !p.HasWindow {
		return nil
	}
	return &scheduler.CountWindow{StartMin: p.WindowStartMin, EndMin: p.WindowEndMin}
}

// upstreamOptions 由配置构建上游热改选项（装配与保存路径共用同一口径，
// 避免两处各写一份导致“热改后与重启后行为不一致”）。
func upstreamOptions(cfg *config.Config) upstream.Options {
	return upstream.Options{
		Profiles:             cfg.Upstream.Profiles,
		ClientName:           cfg.Upstream.ClientName,
		DeviceToken:          cfg.Upstream.DeviceToken,
		DeviceTokenFile:      cfg.Upstream.DeviceTokenFile,
		PassthroughIP:        cfg.Upstream.PassthroughIP,
		DeriveConversationID: cfg.Upstream.DeriveConversationID,
		ChatBaseCN:           cfg.Upstream.ChatBaseCN,
		ChatBaseGlobal:       cfg.Global.ChatBase,
		BillingBaseGlobal:    cfg.Global.BillingBase,
		GlobalEnabled:        cfg.Global.Enabled,
		IdleTimeout:          time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second,
		StreamTimeouts: upstream.StreamTimeoutConfig{
			FirstModelEvent: time.Duration(cfg.Upstream.FirstModelEventSeconds) * time.Second,
			FirstGeneration: time.Duration(cfg.Upstream.FirstGenerationSeconds) * time.Second,
			Tail:            time.Duration(cfg.Upstream.TailSeconds) * time.Second,
		},
		ShortTimeout: time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second,
	}
}

// archiveConfigFrom 由配置构建归档参数（装配与热改共用同一口径）。
// 归档目录固定由**装配期** stateFile 派生：state_file 是重启项，热改归档参数时
// 不能把归档瞬间切到另一个目录（会分裂持久化视图）。
func archiveConfigFrom(stateFile string, c *config.Config) reqlog.Config {
	return reqlog.Config{
		Dir:           stateSibling(stateFile, "request-logs"),
		Enabled:       c.Logging.RequestArchiveEnabled,
		RetentionDays: c.Logging.RequestRetentionDays,
		MaxBytes:      int64(c.Logging.RequestArchiveMaxMB) << 20,
	}
}

// buildScrubLayer 由配置构建出站指纹改写层。
//
// 开关关闭时内置与自定义规则**一并忽略**（不构建、不参与预检）：避免
// "关着开关却仍要付一份哨兵扫描成本"这种反直觉行为。规则非法时返回错误
// （normalize 已校验过，这里是二次防护，供不经 normalize 的调用路径使用）。
func buildScrubLayer(enabled bool, disabled []string, overrides map[string]string, rules []scrub.Rule) (*scrub.Layer, error) {
	return scrub.BuildLayer(enabled, disabled, overrides, rules)
}
