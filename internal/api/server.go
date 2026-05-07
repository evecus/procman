package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/services", s.handleServices)
	mux.HandleFunc("/api/services/", s.handleService)
	mux.HandleFunc("/api/ws", s.handleWS)

	// Static frontend
	mux.Handle("/", http.FileServer(http.Dir("/app/web/static")))

	return mux
}

// ── broadcast ──────────────────────────────────────────────────────────────

func (s *Server) broadcastLoop() {
	for ev := range s.mgr.Events() {
		data, _ := json.Marshal(ev)
		s.clients.Range(func(k, v interface{}) bool {
			conn := k.(*websocket.Conn)
			_ = conn.WriteMessage(websocket.TextMessage, data)
			return true
		})
	}
}

// ── WebSocket ──────────────────────────────────────────────────────────────

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.clients.Store(conn, true)
	defer func() {
		s.clients.Delete(conn)
		conn.Close()
	}()

	// send current state snapshot
	services := s.mgr.List()
	snap, _ := json.Marshal(map[string]interface{}{"type": "snapshot", "data": services})
	_ = conn.WriteMessage(websocket.TextMessage, snap)

	// keep alive
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// ── REST helpers ───────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ── /api/services ──────────────────────────────────────────────────────────

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.mgr.List())

	case http.MethodPost:
		var cfg manager.ServiceConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		if cfg.Name == "" || cfg.Command == "" {
			writeError(w, 400, "name and command required")
			return
		}
		if err := s.mgr.AddService(cfg); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, map[string]string{"status": "created"})

	default:
		writeError(w, 405, "method not allowed")
	}
}

// ── /api/services/{name}[/action] ─────────────────────────────────────────

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	// strip prefix "/api/services/"
	path := strings.TrimPrefix(r.URL.Path, "/api/services/")
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	if name == "" {
		writeError(w, 400, "service name required")
		return
	}

	switch {
	// GET /api/services/{name}
	case action == "" && r.Method == http.MethodGet:
		st, err := s.mgr.Get(name)
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, st)

	// PUT /api/services/{name} — update config
	case action == "" && r.Method == http.MethodPut:
		var cfg manager.ServiceConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		cfg.Name = name
		if err := s.mgr.UpdateService(cfg); err != nil {
			writeError(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "updated"})

	// DELETE /api/services/{name}
	case action == "" && r.Method == http.MethodDelete:
		if err := s.mgr.RemoveService(name); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "removed"})

	// POST /api/services/{name}/start|stop|restart
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

	// GET /api/services/{name}/logs
	case action == "logs" && r.Method == http.MethodGet:
		logs, err := s.mgr.GetLogs(name)
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, logs)

	default:
		writeError(w, 404, fmt.Sprintf("unknown action %q", action))
	}
}

func StartServer(addr string, mgr *manager.Manager) error {
	srv := New(mgr)
	log.Printf("procman listening on %s", addr)
	return http.ListenAndServe(addr, srv.Handler())
}