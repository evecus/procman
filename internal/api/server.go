package api

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/procman/internal/manager"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	mgr     *manager.Manager
	clients sync.Map
}

func New(mgr *manager.Manager) *Server {
	s := &Server{mgr: mgr}
	go s.broadcastLoop()
	return s
}

// StartServer 供 main.go 调用
func StartServer(addr string, mgr *manager.Manager) error {
	s := New(mgr)
	log.Printf("Starting server on %s", addr)
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 1. API 路由
	mux.HandleFunc("/api/services", s.handleServices)
	mux.HandleFunc("/api/services/", s.handleService)
	mux.HandleFunc("/api/ws", s.handleWS)

	// 2. 静态资源路由
	mux.Handle("/", http.FileServer(http.Dir("web/static")))

	webPassword := os.Getenv("WEB_PASSWORD")
	if webPassword == "" {
		return mux
	}

	return s.withBasicAuth(mux, webPassword)
}

// ── WebSocket 广播逻辑 ──────────────────────────────────────────────────────

func (s *Server) broadcastLoop() {
	for ev := range s.mgr.Events() {
		data, _ := json.Marshal(ev)
		s.clients.Range(func(k, v interface{}) bool {
			c := k.(*websocket.Conn)
			if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
				c.Close()
				s.clients.Delete(k)
			}
			return true
		})
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.clients.Store(conn, true)
}

// ── API 处理器 ──────────────────────────────────────────────────────────────

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, s.mgr.List())
		return
	}
	if r.Method == http.MethodPost {
		var cfg manager.ServiceConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		if err := s.mgr.AddService(cfg); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, cfg)
		return
	}
	w.WriteHeader(405)
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	// 路径解析示例: /api/services/my-app/start -> name=my-app, action=start
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/services/"), "/")
	name := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		state, err := s.mgr.Get(name)
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, state)

	case action == "logs" && r.Method == http.MethodGet:
		logs, err := s.mgr.GetLogs(name)
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, logs)

	case action == "start" && r.Method == http.MethodPost:
		if err := s.mgr.Start(name); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "started"})

	case action == "stop" && r.Method == http.MethodPost:
		if err := s.mgr.Stop(name); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "stopped"})

	case action == "restart" && r.Method == http.MethodPost:
		if err := s.mgr.Restart(name); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "restarted"})

	case action == "autostart" && r.Method == http.MethodPost:
		state, err := s.mgr.Get(name)
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		cfg := state.Config
		cfg.AutoStart = !cfg.AutoStart
		if err := s.mgr.UpdateService(cfg); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ok", "auto_start": cfg.AutoStart})

	case action == "" && r.Method == http.MethodDelete:
		if err := s.mgr.RemoveService(name); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "removed"})

	default:
		w.WriteHeader(405)
	}
}

func (s *Server) withBasicAuth(next http.Handler, password string) http.Handler {
	realm := "procman"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, provided, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(provided), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Helper functions ───────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
