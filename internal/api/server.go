// Package api exposes a passwordless visitor HTTP boundary. Workers never receive browser credentials.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/config"
	"github.com/MOLI0711/MOLIVedio/internal/domain"
	db "github.com/MOLI0711/MOLIVedio/internal/repository/mysql"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
	"github.com/google/uuid"
)

type Limiter interface {
	Allow(context.Context, string) (bool, error)
}
type Server struct {
	Config  config.Config
	Store   *db.Store
	Storage storage.Store
	Limiter Limiter
	mux     *http.ServeMux
	Routes  []Route
}
type Route struct{ Method, Path string }
type authKey struct{}
type failure struct {
	Status        int
	Code, Message string
}

func (f *failure) Error() string { return f.Code }

type runConflict struct{ ID string }

func (e *runConflict) Error() string { return "active run conflict" }

type handler func(http.ResponseWriter, *http.Request) error

func New(c config.Config, s *db.Store, objects storage.Store, limiter Limiter) *Server {
	a := &Server{Config: c, Store: s, Storage: objects, Limiter: limiter, mux: http.NewServeMux()}
	a.route("GET /healthz", false, func(w http.ResponseWriter, r *http.Request) error {
		return reply(w, 200, map[string]string{"status": "ok"})
	})
	a.route("GET /readyz", false, func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := s.DB.PingContext(ctx); err != nil {
			return &failure{503, "NOT_READY", "数据库尚未就绪"}
		}
		return reply(w, 200, map[string]string{"status": "ready"})
	})
	a.route("POST /v1/auth/guest", false, a.guest)
	a.route("GET /v1/auth/session", true, a.authSession)
	a.route("POST /v1/auth/logout", true, a.logout)
	a.route("GET /v1/meta", true, func(w http.ResponseWriter, r *http.Request) error {
		return reply(w, 200, map[string]any{"model_mode": c.Model.Mode, "asr_mode": c.ASR.Mode, "search_enabled": c.Search.Enabled})
	})
	a.resources()
	a.uploads()
	return a
}
func (a *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }
func (a *Server) route(pattern string, protected bool, h handler) {
	parts := strings.SplitN(pattern, " ", 2)
	a.Routes = append(a.Routes, Route{parts[0], parts[1]})
	a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.NewString()
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		var err error
		defer func() {
			if p := recover(); p != nil {
				slog.Error("HTTP panic", "request_id", requestID)
				writeError(w, requestID, errors.New("handler panic"))
			}
		}()
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != a.Config.App.PublicOrigin {
				writeError(w, requestID, &failure{403, "ORIGIN_DENIED", "请求来源不被允许"})
				return
			}
		}
		if protected {
			cookie, e := r.Cookie("va_session")
			if e != nil || len(cookie.Value) != 64 {
				writeError(w, requestID, &failure{401, "UNAUTHENTICATED", "访客会话尚未建立，请刷新页面"})
				return
			}
			u, e := a.Store.Authenticate(r.Context(), digest(cookie.Value))
			if e != nil {
				if errors.Is(e, domain.ErrNotFound) {
					e = &failure{401, "UNAUTHENTICATED", "访客会话已失效，请刷新页面"}
				}
				writeError(w, requestID, e)
				return
			}
			if r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf(cookie.Value))) != 1 {
				writeError(w, requestID, &failure{403, "CSRF_INVALID", "安全令牌已失效，请刷新页面"})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), authKey{}, u))
			if a.Limiter != nil && r.Method != "GET" {
				ok, e := a.Limiter.Allow(r.Context(), "user:"+u.ID.String())
				if e != nil {
					writeError(w, requestID, &failure{503, "RATE_LIMIT_UNAVAILABLE", "限流服务暂不可用"})
					return
				}
				if !ok {
					w.Header().Set("Retry-After", "10")
					writeError(w, requestID, &failure{429, "RATE_LIMITED", "请求过于频繁"})
					return
				}
			}
		}
		err = h(w, r)
		if err != nil {
			writeError(w, requestID, err)
		}
	})
}
func user(r *http.Request) *domain.User { return r.Context().Value(authKey{}).(*domain.User) }
func id(r *http.Request, name string) (uuid.UUID, error) {
	v, e := uuid.Parse(r.PathValue(name))
	if e != nil {
		return v, domain.ErrInvalid
	}
	return v, nil
}
func optionalID(v string) (uuid.UUID, error) {
	if v == "" {
		return uuid.Nil, nil
	}
	u, e := uuid.Parse(v)
	if e != nil {
		return u, domain.ErrInvalid
	}
	return u, nil
}
func nonzero(v uuid.UUID) string {
	if v == uuid.Nil {
		return ""
	}
	return v.String()
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return domain.ErrInvalid
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.ErrInvalid
	}
	return nil
}
func reply(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
	return nil
}
func items(w http.ResponseWriter, v any) error { return reply(w, 200, map[string]any{"items": v}) }
func writeError(w http.ResponseWriter, requestID string, err error) {
	var conflict *runConflict
	if errors.As(err, &conflict) {
		_ = reply(w, 409, map[string]any{"error": map[string]any{"code": "ACTIVE_RUN_CONFLICT", "message": "当前会话已有运行中的问答", "details": map[string]string{"current_run_id": conflict.ID}}, "request_id": requestID})
		return
	}
	f := &failure{500, "INTERNAL_ERROR", "服务处理失败，请携带请求编号排查"}
	var explicit *failure
	switch {
	case errors.As(err, &explicit):
		f = explicit
	case errors.Is(err, domain.ErrNotFound):
		f = &failure{404, "NOT_FOUND", "资源不存在或无权访问"}
	case errors.Is(err, domain.ErrInvalid):
		f = &failure{400, "INVALID_ARGUMENT", "请求字段不正确"}
	case errors.Is(err, domain.ErrConflict):
		f = &failure{409, "VERSION_CONFLICT", "请求冲突，请刷新状态后重试"}
	case errors.Is(err, domain.ErrNotReady):
		f = &failure{409, "CONTENT_NOT_READY", "视频尚未完成解析和索引"}
	case errors.Is(err, domain.ErrBudgetExceeded):
		f = &failure{429, "QUOTA_EXCEEDED", "任务配额已用完"}
	}
	if f.Status >= 500 {
		slog.Error("request failed", "request_id", requestID, "error_type", fmtType(err))
	}
	_ = reply(w, f.Status, map[string]any{"error": map[string]string{"code": f.Code, "message": f.Message}, "request_id": requestID})
}

// Do not serialize database/provider errors: they may contain credentials or signed URLs.
func fmtType(err error) string {
	var p *storage.Error
	if errors.As(err, &p) {
		return p.Code
	}
	return "internal"
}
func digest(raw string) string { h := sha256.Sum256([]byte(raw)); return hex.EncodeToString(h[:]) }
func csrf(raw string) string   { return digest("video-agent-csrf:" + raw) }

// Visitors keep a private workspace without creating an account or choosing a password.
// Reusing a valid cookie is essential: repeated initialization must not switch owners.
func (a *Server) guest(w http.ResponseWriter, r *http.Request) error {
	if cookie, err := r.Cookie("va_session"); err == nil && len(cookie.Value) == 64 {
		u, err := a.Store.Authenticate(r.Context(), digest(cookie.Value))
		if err == nil {
			return reply(w, 200, map[string]any{"user": u, "csrf_token": csrf(cookie.Value)})
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	if a.Limiter != nil {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ok, err := a.Limiter.Allow(r.Context(), "guest:"+host)
		if err != nil {
			return &failure{503, "RATE_LIMIT_UNAVAILABLE", "限流服务暂不可用"}
		}
		if !ok {
			w.Header().Set("Retry-After", "60")
			return &failure{429, "RATE_LIMITED", "访客空间创建过于频繁，请稍后重试"}
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	raw := hex.EncodeToString(b)
	cfg, err := json.Marshal(DefaultAgentConfig())
	if err != nil {
		return err
	}
	const ttl = 30 * 24 * time.Hour
	u, err := a.Store.CreateGuestSession(r.Context(), digest(raw), ttl, cfg)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "va_session", Value: raw, Path: "/", HttpOnly: true, Secure: a.Config.App.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds()), Expires: time.Now().Add(ttl)})
	return reply(w, 200, map[string]any{"user": u, "csrf_token": csrf(raw)})
}
func (a *Server) authSession(w http.ResponseWriter, r *http.Request) error {
	cookie, _ := r.Cookie("va_session")
	return reply(w, 200, map[string]any{"user": user(r), "csrf_token": csrf(cookie.Value)})
}
func (a *Server) logout(w http.ResponseWriter, r *http.Request) error {
	cookie, _ := r.Cookie("va_session")
	if e := a.Store.RevokeAuthSession(r.Context(), digest(cookie.Value)); e != nil {
		return e
	}
	http.SetCookie(w, &http.Cookie{Name: "va_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.Config.App.CookieSecure, SameSite: http.SameSiteLaxMode})
	return reply(w, 200, map[string]bool{"ok": true})
}
