// Package registry 是 host/registry 适配器:只读在线仓库服务端。
//
// 从本地目录提供 repo.json / repo.sig.json / artifacts/*.zip;路径限制在 Root 下,
// 可选 Bearer Token 认证。不做签名生成/上传/删除。
package registry

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options 配置只读仓库服务。
type Options struct {
	// Root 是仓库根目录(必须存在)。
	Root string
	// Token 非空时要求 Authorization: Bearer <Token>。
	Token string
	// CertFile/KeyFile 成对出现时 Serve 使用 TLS;只出现一个报错。
	CertFile string
	KeyFile  string
	// RateLimit 是每窗口(1 秒)允许的请求数;0 表示不限。
	RateLimit int
	// Now 可注入时钟(测试用);nil 表示 time.Now。
	Now func() time.Time
	// AllowOrigin 非空时启用 CORS;空表示关闭。
	AllowOrigin string
}

// NewHandler 构造仓库 handler。
func NewHandler(opts Options) (http.Handler, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("host/registry: Root 不能为空")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("host/registry: 解析根目录失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("host/registry: 根目录不是目录: %s", root)
	}
	if opts.RateLimit < 0 {
		return nil, fmt.Errorf("host/registry: RateLimit 不能为负")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &server{root: root, token: opts.Token, rateLimit: opts.RateLimit, now: now, allowOrigin: opts.AllowOrigin}
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.json", s.handleRepoFile("repo.json"))
	mux.HandleFunc("/repo.sig.json", s.handleRepoFile("repo.sig.json"))
	mux.HandleFunc("/artifacts/", s.handleArtifact)
	mux.HandleFunc("/", s.handleIndex)
	return s.corsMiddleware(s.rateLimitMiddleware(s.auth(mux))), nil
}

type server struct {
	root  string
	token string

	rateLimit   int
	now         func() time.Time
	allowOrigin string

	rateMu      sync.Mutex
	windowStart time.Time
	windowCount int
}

// Serve 在 ln 上提供仓库服务,ctx 取消时优雅关闭(返回 nil)。
func Serve(ctx context.Context, opts Options, ln net.Listener) error {
	h, err := NewHandler(opts)
	if err != nil {
		return err
	}
	if (opts.CertFile == "") != (opts.KeyFile == "") {
		return fmt.Errorf("host/registry: CertFile 与 KeyFile 必须成对提供")
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

// corsMiddleware 在 AllowOrigin 非空时设置 CORS 头并处理预检。
func (s *server) corsMiddleware(next http.Handler) http.Handler {
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
func (s *server) rateLimitMiddleware(next http.Handler) http.Handler {
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
			http.Error(w, "请求过于频繁", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) auth(next http.Handler) http.Handler {
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
			http.Error(w, "未认证", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleRepoFile(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
			return
		}
		path, err := s.resolve(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := os.Stat(path); err != nil {
			http.Error(w, "仓库文件不存在", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, path)
	}
}

func (s *server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/artifacts/")
	if name == "" {
		http.Error(w, "缺少 artifact 名", http.StatusBadRequest)
		return
	}
	if strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		http.Error(w, "artifact 路径非法", http.StatusBadRequest)
		return
	}
	path, err := s.resolve(filepath.Join("artifacts", name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.Error(w, "artifact 不存在", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	http.ServeFile(w, r, path)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Error(w, "未知路径", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "routes:\n/repo.json\n/repo.sig.json\n/artifacts/<name>.zip\n")
}

func (s *server) resolve(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("路径不能逃逸仓库根")
	}
	abs, err := filepath.Abs(filepath.Join(s.root, clean))
	if err != nil {
		return "", fmt.Errorf("解析路径失败")
	}
	inside, err := filepath.Rel(s.root, abs)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径不能逃逸仓库根")
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		inside, err := filepath.Rel(s.root, resolved)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("符号链接指向仓库根之外")
		}
		abs = resolved
	}
	return abs, nil
}
