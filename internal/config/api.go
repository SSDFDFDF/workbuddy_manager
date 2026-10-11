// api.go 配置域 HTTP 接口：读取当前配置、校验并保存（热生效 + 重启项标注）、
// 提示词草稿预览。
//
// 归属：配置域的读写接口属于配置域本身（原 internal/panel/config.go）。面板只负责
// 把它挂到 /panel/api/* 并套上鉴权，不再持有配置的读写逻辑——这样"配置长什么样、
// 哪些能热生效"只有一处真相，命令行/脚本/面板共用同一套语义。
//
// 分工：本文件只做 HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包
// （由 main 完成"校验 → 落盘 → 热应用 → 返回需重启字段列表"）、preview 透传给
// PreviewPrompt 闭包（只解析草稿 prompt 段，不落盘）。
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// APIConfig 配置域 HTTP 接口的依赖（main 装配注入）。
type APIConfig struct {
	// ConfigPath 配置文件路径（回显给面板，便于用户定位磁盘上的文件）。
	ConfigPath string
	// LoadConfig 返回解析后的配置对象（前端展示/校验用）。
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表。
	// error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)
	// PreviewPrompt 解析草稿的 prompt 段并返回各域生效规则（"立即预览"用）：
	// 不落盘、不校验其余配置。
	PreviewPrompt func(raw []byte) (any, error)
	// VersionInfo 网关版本信息（面板展示用）；nil 时该字段为空。
	VersionInfo func() any
}

// API 配置域 handler。顶层 mux 按完整路径注册（面板与配置域共用 /panel/api
// 前缀），因此用法是：panel 把这三个 pattern 交给本 handler，而不是自己实现。
type API struct {
	cfg APIConfig
	mux *http.ServeMux
}

// NewAPI 构建配置域 handler；cfg.LoadConfig 为 nil 时对应接口返回 501。
func NewAPI(cfg APIConfig) *API {
	a := &API{cfg: cfg, mux: http.NewServeMux()}
	a.mux.HandleFunc("GET /panel/api/config", a.get)
	a.mux.HandleFunc("GET /panel/api/config/catalog", a.catalog)
	a.mux.HandleFunc("POST /panel/api/config", a.save)
	a.mux.HandleFunc("POST /panel/api/prompt/preview", a.preview)
	return a
}

// ServeHTTP 实现 http.Handler（面板以 withAuth 包装后挂载）。
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// Paths 本 handler 负责的全部路由 pattern（面板据此注册，避免两处各写一份）。
func (a *API) Paths() []string {
	return []string{
		"GET /panel/api/config",
		"GET /panel/api/config/catalog",
		"POST /panel/api/config",
		"POST /panel/api/prompt/preview",
	}
}

// get 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	if a.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := a.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	var vInfo any
	if a.cfg.VersionInfo != nil {
		vInfo = a.cfg.VersionInfo()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"path":         a.cfg.ConfigPath,
		"config":       cfg,
		"version_info": vInfo,
	})
}

// catalog 返回字段目录（internal/config/catalog.go）：面板据此渲染「需重启」
// 徽标与原因，不再在 HTML 里手写一份名单（两份名单必然会漂移）。
func (a *API) catalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"fields": Entries(),
	})
}

// save 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (a *API) save(w http.ResponseWriter, r *http.Request) {
	if a.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := a.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}

// UpdateGrowthAutotasks 更新成长任务配置并热保存落盘。
func (a *API) UpdateGrowthAutotasks(fn func(g *GrowthAutotasks) error) ([]string, error) {
	if a == nil || a.cfg.LoadConfig == nil || a.cfg.SaveConfig == nil {
		return nil, fmt.Errorf("config api not available")
	}
	cfgAny, err := a.cfg.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	cfg, ok := cfgAny.(*Config)
	if !ok {
		raw, err := json.Marshal(cfgAny)
		if err != nil {
			return nil, err
		}
		cfg = &Config{}
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, err
		}
	}
	if err := fn(&cfg.Growth.Autotasks); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return a.cfg.SaveConfig(raw)
}

// preview 解析草稿的 prompt 段并返回各域生效规则（不落盘）。
//
// 面板的"立即预览"按钮走这里：用户在表单里改了 mode/preset/file/text 之后，
// 保存前就能看到 cn/global 两域实际会发出去的正文（含提交截断标记），
// 避免"保存了但不知道发了什么"。
func (a *API) preview(w http.ResponseWriter, r *http.Request) {
	if a.cfg.PreviewPrompt == nil {
		writeErr(w, http.StatusNotImplemented, "prompt preview api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	out, err := a.cfg.PreviewPrompt(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writeJSON / writeErr 与面板同口径的最小 JSON 响应helper（面板 API 的响应
// 形状是 {"ok":…} 信封，配置域作为其中一族接口保持一致）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
