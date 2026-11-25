// Package server 是 host/server 适配器:只读 HTTP API(plan / caps / list)。
//
// 它只做协议映射与路径约束,不做业务;所有路径都必须落在 Options.Root 之下。
// 鉴权可选:Options.Token 非空时要求 Bearer;未配置 Token 时只应绑定 localhost 或由外层网关保护。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/install"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/platform/preset"
)

// Options 配置只读服务器。
type Options struct {
	// Root 是允许访问的根目录(必填,会被解析为绝对路径)。
	Root string
	// Token 非空时,所有路由要求 Authorization: Bearer <Token>。
	Token string
	// RateLimit 是每窗口(1 秒)允许的请求数;0 表示不限。
	RateLimit int
	// Now 可注入时钟(测试用);nil 表示 time.Now。
	Now func() time.Time
	// CertFile/KeyFile 成对出现时 Serve 使用 TLS;只出现一个报错。
	CertFile string
	KeyFile  string
	// AllowOrigin 非空时启用 CORS(Access-Control-Allow-Origin);空表示关闭。
	AllowOrigin string
}

// Serve 在 ln 上提供只读 HTTP API,ctx 取消时优雅关闭(返回 nil)。
func Serve(ctx context.Context, opts Options, ln net.Listener) error {
	h, err := NewHandler(opts)
	if err != nil {
		return err
	}
	if (opts.CertFile == "") != (opts.KeyFile == "") {
		return fmt.Errorf("host/server: CertFile 与 KeyFile 必须成对提供")
	}
	srv := &http.Server{Handler: h}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if opts.CertFile != "" {
		err = srv.ServeTLS(ln, opts.CertFile, opts.KeyFile)
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Server 是只读 HTTP 适配器。
type Server struct {
	root  string
	token string
	mux   *http.ServeMux

	rateLimit   int
	now         func() time.Time
	allowOrigin string

	rateMu      sync.Mutex
	windowStart time.Time
	windowCount int
}

// NewHandler 构造 HTTP handler;根目录必须存在。
func NewHandler(opts Options) (http.Handler, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("host/server: Root 不能为空")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("host/server: 解析根目录失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("host/server: 根目录不是目录: %s", root)
	}
	if opts.RateLimit < 0 {
		return nil, fmt.Errorf("host/server: RateLimit 不能为负")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{root: root, token: opts.Token, mux: http.NewServeMux(), rateLimit: opts.RateLimit, now: now, allowOrigin: opts.AllowOrigin}
	s.mux.HandleFunc("/api/plan", s.handlePlan)
	s.mux.HandleFunc("/api/inspect", s.handleInspect)
	s.mux.HandleFunc("/api/verify", s.handleVerify)
	s.mux.HandleFunc("/api/caps", s.handleCaps)
	s.mux.HandleFunc("/api/list", s.handleList)
	s.mux.HandleFunc("/", s.handleIndex)
	return s.corsMiddleware(s.rateLimitMiddleware(s.authenticate(s.mux))), nil
}

// corsMiddleware 在 AllowOrigin 非空时设置 CORS 头并处理预检。
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	if s.allowOrigin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.allowOrigin)
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware 是固定窗口限流:每 1 秒最多 RateLimit 个请求。
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	if s.rateLimit <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.rateMu.Lock()
		now := s.now()
		if s.windowStart.IsZero() || now.Sub(s.windowStart) >= time.Second {
			s.windowStart = now
			s.windowCount = 0
		}
		s.windowCount++
		over := s.windowCount > s.rateLimit
		s.rateMu.Unlock()
		if over {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate 在 Token 非空时校验 Bearer 令牌(常量时间比较);空 Token 表示不鉴权。
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) ||
			subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未认证"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolve 把相对路径限制在 root 之下(拒绝绝对路径、.. 逃逸与符号链接逃逸)。
func (s *Server) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("缺少 path 参数")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("只允许相对路径")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径不能逃逸根目录")
	}
	abs, err := filepath.Abs(filepath.Join(s.root, clean))
	if err != nil {
		return "", fmt.Errorf("解析路径失败")
	}
	inside, err := filepath.Rel(s.root, abs)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径不能逃逸根目录")
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		inside, err := filepath.Rel(s.root, resolved)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("符号链接指向根目录之外")
		}
		abs = resolved
	}
	return abs, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) onlyGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return false
	}
	return true
}

// loadTarget 按路径加载插件目录或 preset 文件。
func (s *Server) loadTarget(rel string) ([]*plugin.Manifest, error) {
	path, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".preset.json") {
		p, err := preset.Load(path)
		if err != nil {
			return nil, err
		}
		return p.Expand()
	}
	// 安装根目录按 index.json 的 trust 加载;普通插件目录仍走 plugin.LoadDir。
	if _, err := os.Stat(filepath.Join(path, install.IndexFile)); err == nil {
		return install.LoadDir(path)
	}
	return plugin.LoadDir(path)
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if !s.onlyGet(w, r) {
		return
	}
	manifests, err := s.loadTarget(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	report := assemble.Plan(manifests)
	pluginsList := make([]string, 0, len(manifests))
	for _, m := range manifests {
		pluginsList = append(pluginsList, m.String())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       report.OK(),
		"plugins":  pluginsList,
		"order":    report.Order,
		"warnings": report.Warnings,
		"problems": report.Problems,
	})
}

// handleInspect 临时挂载插件并返回快照;注意它会执行插件副作用,仅限本地开发使用。
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if !s.onlyGet(w, r) {
		return
	}
	manifests, err := s.loadTarget(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := builtins.ApplyAutoGrants(manifests); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	plan := assemble.Plan(manifests)
	if !plan.OK() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "计划不可执行", "problems": plan.Problems})
		return
	}
	app := kernel.New(kernel.Options{})
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	up, err := assemble.Up(app, manifests, plan, lookup)
	if err != nil {
		_ = app.Stop(context.Background())
		writeError(w, http.StatusBadRequest, err)
		return
	}
	snap := app.Inspect()
	stopErr := app.Stop(context.Background())

	type fiberRow struct {
		ID     string `json:"id"`
		Plugin string `json:"plugin"`
		State  string `json:"state"`
		Parent string `json:"parent,omitempty"`
	}
	type serviceRow struct {
		Name       string   `json:"name"`
		Provider   string   `json:"provider"`
		Dependents []string `json:"dependents,omitempty"`
	}
	type effectRow struct {
		Plugin string `json:"plugin"`
		Label  string `json:"label"`
	}
	payload := struct {
		Order    []string     `json:"order"`
		Fibers   []fiberRow   `json:"fibers"`
		Services []serviceRow `json:"services"`
		Effects  []effectRow  `json:"effects"`
		Events   []string     `json:"events"`
		StopErr  string       `json:"stopError,omitempty"`
	}{Order: up.Order, Events: snap.Events}
	for _, f := range snap.Fibers {
		payload.Fibers = append(payload.Fibers, fiberRow{ID: f.ID, Plugin: f.Plugin, State: f.State, Parent: f.Parent})
	}
	for _, sv := range snap.Services {
		payload.Services = append(payload.Services, serviceRow{Name: sv.Name, Provider: sv.Provider, Dependents: sv.Dependents})
	}
	for _, e := range snap.Effects {
		payload.Effects = append(payload.Effects, effectRow{Plugin: e.Plugin, Label: e.Label})
	}
	if stopErr != nil {
		payload.StopErr = stopErr.Error()
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleVerify 对插件目录做 VerifyDir(清单/摘要/编译/信任/签名),不落盘。
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !s.onlyGet(w, r) {
		return
	}
	path, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := install.VerifyDir(path, install.Options{})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	capsList := info.Manifest.Caps
	writeJSON(w, http.StatusOK, map[string]any{
		"id": info.ID, "version": info.Version, "kind": info.Kind,
		"digest": info.Digest, "trust": string(info.Trust), "signed": info.Signed,
		"caps": capsList,
	})
}

func (s *Server) handleCaps(w http.ResponseWriter, r *http.Request) {
	if !s.onlyGet(w, r) {
		return
	}
	manifests, err := s.loadTarget(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	broker := caps.NewBroker(nil)
	grantErr := assemble.GrantManifests(broker, manifests)
	type row struct {
		ID      string   `json:"id"`
		Version string   `json:"version"`
		Trust   string   `json:"trust"`
		Caps    []string `json:"caps"`
	}
	rows := make([]row, 0, len(manifests))
	for _, m := range manifests {
		trust := m.Trust
		if trust == "" {
			trust = string(caps.TrustThirdParty)
		}
		rows = append(rows, row{ID: m.ID, Version: m.Version, Trust: trust, Caps: m.Caps})
	}
	payload := map[string]any{"ok": grantErr == nil, "plugins": rows}
	if grantErr != nil {
		payload["error"] = grantErr.Error()
		writeJSON(w, http.StatusOK, payload)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if !s.onlyGet(w, r) {
		return
	}
	rel := r.URL.Query().Get("dest")
	if rel == "" {
		rel = "."
	}
	path, err := s.resolve(rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	entries, err := install.List(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "未知路径"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"routes": []string{"/api/plan?path=", "/api/caps?path=", "/api/list?dest="},
	})
}
