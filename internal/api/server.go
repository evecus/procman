package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/procman/internal/manager"
	"github.com/procman/internal/terminal"
	"github.com/procman/web"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	mgr         *manager.Manager
	clients     sync.Map
	sessions    sync.Map
	webPassword string
}

func New(mgr *manager.Manager) *Server {
	s := &Server{
		mgr:         mgr,
		webPassword: os.Getenv("WEB_PASSWORD"),
	}
	go s.broadcastLoop()
	go s.cleanupSessions()
	return s
}

func StartServer(addr string, mgr *manager.Manager) error {
	s := New(mgr)
	log.Printf("Starting server on %s", addr)
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/api/auth/check", s.handleAuthCheck)

	mux.HandleFunc("/api/services", s.withAuth(s.handleServices))
	mux.HandleFunc("/api/services/", s.withAuth(s.handleService))
	mux.HandleFunc("/api/ws", s.withAuth(s.handleWS))
	mux.HandleFunc("/api/terminal", s.withAuth(terminal.HandleWS))

	mux.Handle("/", http.FileServer(http.FS(web.StaticFS)))

	return mux
}

func (s *Server) generateToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) isValidToken(token string) bool {
	if token == "" {
		return false
	}
	v, ok := s.sessions.Load(token)
	if !ok {
		return false
	}
	if time.Now().After(v.(time.Time)) {
		s.sessions.Delete(token)
		return false
	}
	return true
}

func (s *Server) tokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie("pm_token"); err == nil {
		return c.Value
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.webPassword == "" {
			next(w, r)
			return
		}
		token := s.tokenFromRequest(r)
		if !s.isValidToken(token) {
			if r.Header.Get("Upgrade") == "websocket" {
				http.Error(w, "Unauthorized", 401)
				return
			}
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) cleanupSessions() {
	for range time.Tick(10 * time.Minute) {
		s.sessions.Range(func(k, v interface{}) bool {
			if time.Now().After(v.(time.Time)) {
				s.sessions.Delete(k)
			}
			return true
		})
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if s.webPassword == "" {
		writeJSON(w, 200, map[string]string{"status": "ok", "token": "noauth"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.webPassword)) != 1 {
		writeError(w, 401, "wrong password")
		return
	}
	token := s.generateToken()
	s.sessions.Store(token, time.Now().Add(24*time.Hour))
	http.SetCookie(w, &http.Cookie{
		Name:     "pm_token",
		Value:    token,
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]string{"status": "ok", "token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := s.tokenFromRequest(r)
	if token != "" {
		s.sessions.Delete(token)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "pm_token",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if s.webPassword == "" {
		writeJSON(w, 200, map[string]interface{}{"authenticated": true, "required": false})
		return
	}
	token := s.tokenFromRequest(r)
	writeJSON(w, 200, map[string]interface{}{"authenticated": s.isValidToken(token), "required": true})
}

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

	case action == "" && r.Method == http.MethodPut:
		var cfg manager.ServiceConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		cfg.Name = name
		if err := s.mgr.UpdateService(cfg); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, cfg)

	default:
		w.WriteHeader(405)
	}
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
