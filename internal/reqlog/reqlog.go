// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Path             string    `json:"path"`
	Account          string    `json:"account,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	OK               bool      `json:"ok"`
	Outcome          string    `json:"outcome"`
	DurationMs       int64     `json:"duration_ms"`
	TTFBMs           int64     `json:"ttfb_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	Credit           float64   `json:"credit,omitempty"`
	HasCredit        bool      `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`

	// Dropped 是跨协议入口（Responses / Messages）接受但无法表达、因此被忽略的
	// 客户端字段，已去重与限长。用途是真实客户端排障：回答“它发了什么被我们丢了”。
	Dropped []string `json:"dropped,omitempty"`

	// Messages input translation only: counters contain no reasoning/signatures.
	// Replayed counts describe the translated request, not upstream acceptance or
	// cache hits; inspect Attempts/Outcome/usage for delivery and actual effects.
	ThinkingReplayEnabled  bool `json:"thinking_replay_enabled,omitempty"`
	ThinkingPlainBlocks    int  `json:"thinking_plain_blocks,omitempty"`
	ThinkingReplayedBlocks int  `json:"thinking_replayed_blocks,omitempty"`
	ThinkingReplayedBytes  int  `json:"thinking_replayed_bytes,omitempty"`
	ThinkingRedactedBlocks int  `json:"thinking_redacted_blocks,omitempty"`

	// The last completed upstream attempt's classification/code. Replaced as a
	// pair on each attempt; cleared on success. This is not an attempt history.
	UpstreamKind string `json:"upstream_kind,omitempty"`
	UpstreamCode string `json:"upstream_code,omitempty"`

	// ConversationDerived 本次出站的 X-Conversation-ID 是否由网关派生（客户端没给
	// 会话 id、且 upstream.derive_conversation_id 开启）。A/B 观测会话身份策略用。
	ConversationDerived bool `json:"conversation_derived,omitempty"`

	// Rewritten 是网关**主动改写**的生成参数（key:原值→出站值）。当前两类：越界
	// reasoning effort 归一；模型原生工具调用标记还原（正文里的标记 → tool_calls）。
	// 与 Dropped 分开的原因：
	//   - 语义不同：Dropped 是“没执行”，Rewritten 是“按能力换了值执行了”；
	//   - 优先级不同：Dropped 会被客户端的遥测字段（Codex 一次发 6~7 个）占满上限，
	//     若共用一个列表，最需要被看见的改写会静默消失。
	Rewritten []string `json:"rewritten,omitempty"`

	// 出站提示词指纹（仅在 prompt.mode 非 none 时填）。
	//
	// 为什么需要：11128/内容审核类问题复盘时必须能回答"这次到底发出去的是什么形状"。
	// 正文本身不入档（体积 + 隐私：正文里可能含用户业务内容），但**指纹可复现**——
	// 相同 mode/preset/首行/sha256 即"发出去的是同一份内容"，配合面板预览即可还原逐字全文。
	PromptMode   string `json:"prompt_mode,omitempty"`
	PromptPreset string `json:"prompt_preset,omitempty"`
	// PromptSHA 渲染后出站 system 正文的 sha256 前 12 位（含首行改写结果）。
	PromptSHA string `json:"prompt_sha256,omitempty"`
	// PromptChars 渲染后正文的字符数（rune，与面板预览同口径）。
	PromptChars int `json:"prompt_chars,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
//
// archive 用 atomic.Pointer 持有**不可变快照**：面板热改 logging.request_archive_*
// 时整体换一个新 writer（关闭旧的），请求热路径 Load 一次拿到一致视图，无锁无竞争。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	// recent 按完成顺序写入环形缓冲；仅 Snapshot 按最新在前复制。
	recent      [recentLimit]Event
	recentNext  int
	recentCount int
	archive     atomic.Pointer[archiveWriter]
	// closed 置位后 Reconfigure 不再新建 writer（进程关停阶段保存配置时不留
	// 无人回收的归档 goroutine）。
	closed atomic.Bool
}

// normalizeArchiveConfig 归一化归档参数（New / Reconfigure 共用，保证两者口径一致）。
func normalizeArchiveConfig(cfg Config) Config {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	return cfg
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	r := &Recorder{started: time.Now()}
	r.archive.Store(newArchiveWriter(normalizeArchiveConfig(cfg)))
	return r
}

// Reconfigure 在线替换归档参数（Enabled / Dir / RetentionDays / MaxBytes）。
//
// 语义：Enabled=false（或 Dir 为空）= 停止归档，只保留内存指标；重新启用会新建
// writer 并重新打开当天文件。调用方为配置保存路径，不在请求热路径上。
//
// 新旧 writer 的交接顺序：先建新、再换指针、最后关旧。这样"新事件不丢"（换指针后
// Record 一定拿到新 writer）；旧 writer 只剩队列里已收下的事件，排空后退出。两者
// 可能短暂同时持有同一目录的文件句柄，归档文件以 O_APPEND 打开，追加写入互不破坏
// 行完整性。若改成"先关旧再建新"，关旧的这段时间事件会因为 writer 退休而被丢弃。
//
// 返回是否真的发生了替换：配置未变时是空操作（避免无谓的 goroutine 抖动）。
// 内存指标（recent/计数）不受影响，跨重配置保留。
func (r *Recorder) Reconfigure(cfg Config) bool {
	if r == nil || r.closed.Load() {
		return false
	}
	cfg = normalizeArchiveConfig(cfg)
	if old := r.archive.Load(); old != nil && old.sameConfig(cfg) {
		return false
	}
	next := newArchiveWriter(cfg)
	old := r.archive.Swap(next)
	if old != nil {
		old.close()
	}
	return true
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	r.recent[r.recentNext] = e
	r.recentNext = (r.recentNext + 1) % recentLimit
	if r.recentCount < recentLimit {
		r.recentCount++
	}
	r.mu.Unlock()
	if w := r.archive.Load(); w != nil {
		// Reconfigure 可能恰好在此刻换掉 writer：退休的 writer 会拒收（enqueue 返回
		// false），此时用当前指针重试一次，避免这次归档记录凭空消失。
		if !w.enqueue(e) {
			if next := r.archive.Load(); next != nil && next != w {
				next.enqueue(e)
			}
		}
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
	}
	if r.recentCount > 0 {
		s.Recent = make([]Event, r.recentCount)
		for i := range s.Recent {
			s.Recent[i] = r.recent[(r.recentNext-1-i+recentLimit)%recentLimit]
		}
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if w := r.archive.Load(); w != nil {
		s.Archive = w.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil {
		return nil, nil
	}
	w := r.archive.Load()
	if w == nil {
		return nil, nil
	}
	return w.read(limit, filter)
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closed.Store(true)
	if w := r.archive.Load(); w != nil {
		w.close()
	}
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	// closed 在 close() 关闭 run goroutine 之前置位：请求热路径与 Reconfigure
	// 并发时，已退休 writer 不再收事件（否则会静默丢进无人消费的缓冲）。
	closed  atomic.Bool
	dropped atomic.Uint64

	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

// accepting 报告 writer 是否仍接收新事件：已退休（close 后）不再接收，
// 否则事件会静默落进无人消费的缓冲。stats/read 仍按 enabled() 口径保留可观测性。
func (w *archiveWriter) accepting() bool {
	return w.enabled() && !w.closed.Load()
}

// sameConfig 报告两份归档配置（已归一化）是否等价：等价则 Reconfigure 空操作。
func (w *archiveWriter) sameConfig(cfg Config) bool {
	if w == nil {
		return false
	}
	return w.cfg.Enabled == cfg.Enabled && w.cfg.Dir == cfg.Dir &&
		w.cfg.RetentionDays == cfg.RetentionDays && w.cfg.MaxBytes == cfg.MaxBytes &&
		w.cfg.FileMaxBytes == cfg.FileMaxBytes && w.cfg.QueueSize == cfg.QueueSize
}

// enqueue 投入归档队列。返回 false 表示该 writer 已退休、本次事件未被收下
// （调用方应改投当前 writer）；队列满仍返回 true（事件被丢弃并计入 dropped）。
func (w *archiveWriter) enqueue(e Event) bool {
	if !w.accepting() {
		return false
	}
	select {
	case w.ch <- e:
		return true
	default:
		w.dropped.Add(1)
		return true
	}
}

// pruneInterval 目录保留/容量清理的扫描间隔。
//
// flush（bufio 去缓冲）需要秒级时效，但 prune 是 ReadDir + 逐文件 Info + 排序：
// 保留期（天级）与容量上限（MB 级）都不需要秒级判定，此前两者共用 1s ticker，
// 等于每秒白白扫一遍目录。拆开后 flush 仍 1s、prune 降到分钟级。
// var 而非 const：仅便于测试注入短间隔。
var pruneInterval = time.Minute

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastPrune := time.Now()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case <-ticker.C:
			w.flush()
			if now := time.Now(); now.Sub(lastPrune) >= pruneInterval {
				w.prune()
				lastPrune = now
			}
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := "requests-" + day
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		idx := 0
		if name[len(prefix):len(name)-len(".jsonl")] != "" {
			part := strings.TrimSuffix(strings.TrimPrefix(name[len(prefix):], "."), ".jsonl")
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			idx = n
		}
		if idx > bestIndex {
			bestIndex = idx
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return best
}

func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, "requests-"+day)
	path := base + ".jsonl"
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d.jsonl", base, i)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime.Equal(items[j].mtime) {
			return items[i].path < items[j].path
		}
		return items[i].mtime.Before(items[j].mtime)
	})
	for _, it := range items {
		if it.path != w.path && (it.mtime.Before(cutoff) || total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
			}
		}
	}
}

func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗），
	// os.ReadDir 的字典序又会把装着最早事件的基准文件 requests-<day>.jsonl 排在
	// requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	//
	// 内存有界：旧实现把每个文件的全部匹配事件 append 进 out 再全量排序，
	// 只取 100 条也会把整个归档读进内存（实测 21.8MB 归档一次查询分配 208MB）。
	// 现在用固定容量 top-K 堆：内存 O(limit)，扫描量不变。
	top := newTopK(limit)
	var firstErr error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if err := readFile(filepath.Join(w.cfg.Dir, name), filter, top.push); err != nil && firstErr == nil {
			// 单个文件损坏/被截断：返回已有的最近记录 + 错误（旧实现同样返回部分结果）。
			firstErr = err
		}
	}
	return top.sorted(), firstErr
}

// topK 固定容量地保留「按事件时间倒序」最前面的 K 条记录。
//
// 排序契约与旧实现（收集全部 + sort.SliceStable 按时间倒序 + 截断）一致：
// 时间倒序；时间相同时保留**先扫描到**的记录（旧 stable sort 保留 append 顺序，
// 即文件名字典序 + 文件内行序）。内部用小根堆，堆顶是“按该顺序最靠后”的一条，
// 新记录优于堆顶时替换。
//
// 为什么不是遍历完再 sort：K 默认 200 / 上限 1000，而归档可以到几十万行；
// 全量持有既放大内存又拉长 GC，且完全没必要。
func newTopK(limit int) *topK {
	return &topK{limit: limit, heap: make([]topKEntry, 0, limit+1)}
}

type topKEntry struct {
	ev  Event
	seq int
}

type topK struct {
	limit int
	seq   int
	heap  []topKEntry
}

// worse 报告 a 是否应排在 b 之后（时间更早；同时刻扫描更晚）。
func worse(a, b topKEntry) bool {
	if !a.ev.Time.Equal(b.ev.Time) {
		return a.ev.Time.Before(b.ev.Time)
	}
	return a.seq > b.seq
}

func (t *topK) push(e Event) {
	entry := topKEntry{ev: e, seq: t.seq}
	t.seq++
	if len(t.heap) < t.limit {
		t.heap = append(t.heap, entry)
		t.up(len(t.heap) - 1)
		return
	}
	if !worse(t.heap[0], entry) {
		return // 堆顶不差于新记录（即新记录不比已有的更靠前）：丢弃，零分配
	}
	t.heap[0] = entry
	t.down(0)
}

func (t *topK) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !worse(t.heap[i], t.heap[parent]) {
			break
		}
		t.heap[i], t.heap[parent] = t.heap[parent], t.heap[i]
		i = parent
	}
}

func (t *topK) down(i int) {
	n := len(t.heap)
	for {
		left, right := 2*i+1, 2*i+2
		smallest := i
		if left < n && worse(t.heap[left], t.heap[smallest]) {
			smallest = left
		}
		if right < n && worse(t.heap[right], t.heap[smallest]) {
			smallest = right
		}
		if smallest == i {
			return
		}
		t.heap[i], t.heap[smallest] = t.heap[smallest], t.heap[i]
		i = smallest
	}
}

// sorted 按「时间倒序 + 同时刻扫描序升序」输出，与旧 stable sort 结果逐位一致。
func (t *topK) sorted() []Event {
	entries := t.heap
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].ev.Time.Equal(entries[j].ev.Time) {
			return entries[i].ev.Time.After(entries[j].ev.Time)
		}
		return entries[i].seq < entries[j].seq
	})
	out := make([]Event, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ev)
	}
	return out
}

func readFile(path string, filter Filter, sink func(Event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(e) {
			continue
		}
		sink(e)
	}
	return scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.Model, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	s := ArchiveStats{Enabled: w.enabled(), Dir: w.cfg.Dir, DroppedWrites: w.dropped.Load(), LastError: w.errString()}
	if !s.Enabled {
		return s
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			s.Files++
			s.Bytes += info.Size()
		}
	}
	return s
}

func (w *archiveWriter) close() {
	if w == nil || w.done == nil {
		return
	}
	w.closeOnce.Do(func() {
		// 先置 closed（拦住新事件），再关 stop 让 run 排空退出。
		w.closed.Store(true)
		close(w.stop)
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
