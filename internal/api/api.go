// Package api 提供选手接口、管理接口与静态托管。
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"wordle/internal/auth"
	"wordle/internal/config"
	"wordle/internal/game"
)

type Server struct {
	cfg      *config.Config
	game     *game.Game
	auth     *auth.Auth
	frontend fs.FS // 内置前端：index.html / app.js / common.js / base.css / style.css / admin.html / api.html
	log      *log.Logger

	mu         sync.Mutex
	adminToken string // 口令只有一个，令牌就只留一个：后一次登录顶掉前一次
}

// New 组装 Server。frontend 是 go:embed 进来的前端资源（见 web 包）；
// 二进制自带全部静态文件，运行时不读磁盘上的 HTML/JS/CSS。
func New(cfg *config.Config, g *game.Game, a *auth.Auth, frontend fs.FS, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		cfg: cfg, game: g, auth: a,
		frontend: frontend, log: logger,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// 选手侧
	mux.Handle("GET /api/v1/round", s.player(s.handleRound))
	mux.Handle("POST /api/v1/guesses", s.player(s.handleGuess))
	mux.Handle("GET /api/v1/leaderboard", s.player(s.handleLeaderboard))

	// 管理侧
	mux.HandleFunc("POST /api/v1/admin/login", s.handleAdminLogin)
	mux.Handle("GET /api/v1/admin/state", s.admin(s.handleAdminState))
	mux.Handle("POST /api/v1/admin/round/open", s.admin(s.handleAdminOpen))
	mux.Handle("POST /api/v1/admin/round/close", s.admin(s.handleAdminClose))

	// 鉴权
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)

	// 静态前端；/admin 是 admin.html 的别名，见 staticHandler
	mux.Handle("GET /", s.staticHandler())

	return s.logRequests(mux)
}

// ---------- 中间件 ----------

type playerFn func(w http.ResponseWriter, r *http.Request, sub, name string)

// player 要求合法 HMAC token。401 时前端重走 /auth/login，不打扰用户。
func (s *Server) player(h playerFn) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, name, err := s.auth.Verify(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r, sub, name)
	})
}

func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		want := s.adminToken
		s.mu.Unlock()
		got := r.Header.Get("X-Admin-Token")
		if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	})
}

// logRequests 记访问日志。注意：只记 r.URL.Path，绝不记 query
// （/auth/callback 的 code、回跳地址里的 token 都只在 query 里）。
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ---------- 选手接口 ----------

func (s *Server) handleRound(w http.ResponseWriter, r *http.Request, _, _ string) {
	writeJSON(w, http.StatusOK, s.game.Status())
}

func (s *Server) handleGuess(w http.ResponseWriter, r *http.Request, sub, name string) {
	var req struct {
		Guess string `json:"guess"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_length")
		return
	}
	guess := strings.ToLower(strings.TrimSpace(req.Guess))

	outcome, err := s.game.Guess(sub, name, guess, time.Now().UnixMilli())
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request, sub, _ string) {
	entries, me, no := s.game.Leaderboard(sub)
	writeJSON(w, http.StatusOK, map[string]any{"roundNo": no, "entries": entries, "me": me})
}

func writeGameError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, game.ErrBadLength), errors.Is(err, game.ErrNotInWordList), errors.Is(err, game.ErrAlreadyGuessed),
		errors.Is(err, game.ErrBadMode), errors.Is(err, game.ErrBadDuration):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, game.ErrRoundNotOpen), errors.Is(err, game.ErrExpired), errors.Is(err, game.ErrAlreadySolved):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, game.ErrTooManyRequests):
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}

// ---------- 管理接口 ----------

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.cfg.Server.AdminPassword)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	buf := make([]byte, 16)
	rand.Read(buf)
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.adminToken = token
	s.mu.Unlock()
	s.log.Printf("管理面板登录成功")
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleAdminState 管理面板用：场次概览 + 实时榜单。投屏就投这一页，
// 答对题数就在榜单里，不需要另开一个公开只读接口。
func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.game.AdminState())
}

// handleAdminOpen 开新一场。请求体可省：没有 mode 就按轮次赛开，
// 限时赛则必须给 durationSec。
//
//	{"mode":"round"}                         轮次赛：一轮一题，解出即收工
//	{"mode":"timed","durationSec":600}       限时赛：10 分钟，答对一题继续下一题
func (s *Server) handleAdminOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode        string `json:"mode"`
		DurationSec int64  `json:"durationSec"`
	}
	switch err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); {
	case err == nil, errors.Is(err, io.EOF): // 空体 = 老前端，按默认玩法
	default:
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}

	mode := game.Mode(req.Mode)
	if mode == "" {
		mode = game.ModeRound
	}
	no, state, err := s.game.Open(time.Now().UnixMilli(), mode, req.DurationSec*1000)
	if err != nil {
		writeGameError(w, err)
		return
	}
	s.log.Printf("开赛 round=%d mode=%s duration=%ds", no, mode, req.DurationSec)
	writeJSON(w, http.StatusOK, map[string]any{"roundNo": no, "state": state, "mode": mode})
}

func (s *Server) handleAdminClose(w http.ResponseWriter, r *http.Request) {
	state := s.game.Close()
	s.log.Printf("收场 state=%s", state)
	writeJSON(w, http.StatusOK, map[string]any{"state": state})
}

// ---------- 鉴权入口 ----------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, nonce := s.auth.NewState()
	url, err := s.auth.LoginURL(r.Context(), state, nonce)
	if err != nil {
		s.log.Printf("生成登录地址失败: %v", err)
		http.Error(w, "Keycloak 暂不可用，请稍后重试", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	// 回跳地址的 query 里有 code 与 state，别把它泄给下一个站点。
	w.Header().Set("Referrer-Policy", "no-referrer")

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "Keycloak 返回错误: "+e, http.StatusBadRequest)
		return
	}
	nonce, err := s.auth.CheckState(q.Get("state"))
	if err != nil {
		http.Error(w, "state 校验失败（请从本站重新发起登录）", http.StatusBadRequest)
		return
	}
	sub, name, err := s.auth.Exchange(r.Context(), q.Get("code"), nonce)
	if err != nil {
		s.log.Printf("登录失败: %v", err)
		http.Error(w, "登录失败，请重试", http.StatusUnauthorized)
		return
	}
	token := s.auth.Issue(sub, name, time.Now())
	http.Redirect(w, r, "/?token="+token, http.StatusFound)
}

// ---------- 静态托管 ----------

// 静态前端来自 go:embed（web.FS），没有构建步骤：改 HTML/CSS/JS 后重新编译即可生效。
func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		// path.Clean 已把 /admin/ 归一成 /admin，所以一个别名同时覆盖两种写法。
		switch name {
		case "", ".":
			name = "index.html"
		case "admin":
			name = "admin.html"
		}
		// embed 的文件没有 modtime，给不出 Last-Modified/ETag：显式要求每次回源校验，
		// 免得重新编译上线后浏览器还拿着旧 CSS/JS。
		w.Header().Set("Cache-Control", "no-cache")
		// 只服务真实存在的文件；目录不给列表（根路径和管理页别名已在上面换成实际文件）。
		fi, err := fs.Stat(s.frontend, name)
		if err != nil || fi.IsDir() {
			// 接口/鉴权/题库这些机器路径没有对应文件时给 JSON 404：脚本不该拿到一份 HTML。
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") ||
				strings.HasPrefix(r.URL.Path, "/words/") {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, s.frontend, name)
	})
}

// ---------- 输出 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
