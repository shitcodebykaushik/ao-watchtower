// Package web delivers the local, server-rendered Watchtower dashboard.
package web

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
	"github.com/agent-orchestrator/ao-watchtower/internal/ledger"
	"github.com/agent-orchestrator/ao-watchtower/internal/service"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	ledger *ledger.Ledger
	life   *service.Lifecycle
	admin  []byte
	demo   bool
	tmpl   *template.Template
}

func New(l *ledger.Ledger, life *service.Lifecycle, adminToken string, demo bool) (*Server, error) {
	if l == nil || life == nil || adminToken == "" {
		return nil, fmt.Errorf("ledger, lifecycle, and admin token are required")
	}
	t, e := template.New("dashboard").Parse(page)
	if e != nil {
		return nil, e
	}
	return &Server{l, life, []byte(adminToken), demo, t}, nil
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		w.WriteHeader(200)
	})
	m.HandleFunc("/", s.dashboard)
	m.HandleFunc("/api/triggers", s.trigger)
	m.HandleFunc("/api/automation", s.automation)
	return m
}
func now() time.Time { return time.Now() }
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	d, e := s.ledger.Dashboard(r.Context())
	if e != nil {
		http.Error(w, "dashboard unavailable", 500)
		return
	}
	_ = s.tmpl.Execute(w, struct {
		ledger.Dashboard
		Demo bool
	}{d, s.demo})
}
func (s *Server) trigger(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/triggers" {
		http.NotFound(w, r)
		return
	}
	key := domain.TriggerKey(r.URL.Query().Get("trigger"))
	if key == "" {
		http.Error(w, "invalid trigger", 400)
		return
	}
	switch r.URL.Query().Get("action") {
	case "diagnosis":
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if !s.callbackAuth(r, key) {
			http.Error(w, "unauthorized", 401)
			return
		}
		raw, e := readBody(w, r, 64<<10)
		if e != nil {
			return
		}
		valid, e := s.life.SubmitDiagnosis(r.Context(), key, raw)
		if e != nil {
			http.Error(w, "unable to record diagnosis", 500)
			return
		}
		writeJSON(w, map[string]bool{"valid": valid})
	case "approve":
		if r.Method != http.MethodPost || !s.adminAuth(w, r) {
			return
		}
		var v struct {
			Actor string `json:"actor"`
		}
		if !decodeBody(w, r, &v) {
			return
		}
		if len(v.Actor) > 128 || strings.TrimSpace(v.Actor) == "" {
			http.Error(w, "invalid actor", 400)
			return
		}
		if e := s.life.ApproveFix(r.Context(), key, v.Actor); e != nil {
			http.Error(w, "approval unavailable", 409)
			return
		}
		w.WriteHeader(204)
	case "fix":
		if r.Method != http.MethodPost || !s.adminAuth(w, r) {
			return
		}
		if e := s.life.FixWithAO(r.Context(), key); e != nil {
			http.Error(w, "fix unavailable", 409)
			return
		}
		w.WriteHeader(202)
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) automation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !s.adminAuth(w, r) {
		return
	}
	var v struct {
		Disabled bool   `json:"disabled"`
		Actor    string `json:"actor"`
	}
	if !decodeBody(w, r, &v) {
		return
	}
	if len(v.Actor) > 128 || strings.TrimSpace(v.Actor) == "" {
		http.Error(w, "invalid actor", 400)
		return
	}
	if e := s.ledger.SetAutomationDisabled(r.Context(), v.Disabled, v.Actor, now()); e != nil {
		http.Error(w, "unable to update automation", 500)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) adminAuth(w http.ResponseWriter, r *http.Request) bool {
	v := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(v, p) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(v, p)), s.admin) != 1 {
		http.Error(w, "unauthorized", 401)
		return false
	}
	return true
}
func (s *Server) callbackAuth(r *http.Request, key domain.TriggerKey) bool {
	v := r.Header.Get("Authorization")
	return strings.HasPrefix(v, "Bearer ") && s.life.VerifyCallbackToken(key, strings.TrimPrefix(v, "Bearer "))
}
func readBody(w http.ResponseWriter, r *http.Request, n int64) ([]byte, error) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, n))
	if e != nil {
		http.Error(w, "invalid request", 400)
	}
	return b, e
}
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, e := readBody(w, r, 4096)
	if e != nil {
		return false
	}
	if json.Unmarshal(b, v) != nil {
		http.Error(w, "invalid JSON", 400)
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
