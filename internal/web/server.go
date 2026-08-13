// Package web delivers the local, server-rendered Watchtower dashboard.
package web

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/service"
)

type Server struct {
	ledger *ledger.Ledger
	life   *service.Lifecycle
	admin  []byte
	demo   bool
	now    func() time.Time
}

func New(l *ledger.Ledger, life *service.Lifecycle, adminToken string, demo bool) (*Server, error) {
	if l == nil || life == nil || adminToken == "" {
		return nil, fmt.Errorf("ledger, lifecycle, and admin token are required")
	}
	return &Server{ledger: l, life: life, admin: []byte(adminToken), demo: demo, now: time.Now}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", s.dashboard)
	mux.HandleFunc("/api/state", s.state)
	mux.HandleFunc("/api/triggers", s.trigger)
	mux.HandleFunc("/api/automation", s.automation)
	return mux
}

// dashboard serves the static application shell only. Ledger contents, which
// include agent prose and repository paths, are served exclusively by /api/state
// behind the admin token rather than to anything that can reach the port.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	page := shell
	if s.demo {
		page = strings.Replace(page, "data-demo=\"false\"", "data-demo=\"true\"", 1)
	}
	_, _ = io.WriteString(w, page)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.adminAuth(w, r) {
		return
	}
	dashboard, err := s.ledger.Dashboard(r.Context())
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	stats, err := s.ledger.Stats(r.Context())
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	at := s.now()
	payload := State{Demo: s.demo, AutomationDisabled: dashboard.AutomationDisabled, Stats: newStatsPayload(stats), Rows: make([]Row, 0, len(dashboard.Rows))}
	for _, source := range dashboard.Rows {
		payload.Rows = append(payload.Rows, newRow(source, dashboard.AutomationDisabled, at))
	}
	writeJSON(w, payload)
}

func (s *Server) trigger(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/triggers" {
		http.NotFound(w, r)
		return
	}
	key := domain.TriggerKey(r.URL.Query().Get("trigger"))
	if key == "" {
		http.Error(w, "invalid trigger", http.StatusBadRequest)
		return
	}
	switch r.URL.Query().Get("action") {
	case "diagnosis":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !s.callbackAuth(r, key) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		raw, err := readBody(w, r, ledger.MaxDiagnosisRaw)
		if err != nil {
			return
		}
		valid, err := s.life.SubmitDiagnosis(r.Context(), key, raw)
		if err != nil {
			http.Error(w, "unable to record diagnosis", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"valid": valid})
	case "approve":
		if r.Method != http.MethodPost || !s.adminAuth(w, r) {
			return
		}
		actor, ok := decodeActor(w, r)
		if !ok {
			return
		}
		if err := s.life.ApproveFix(r.Context(), key, actor); err != nil {
			http.Error(w, "approval unavailable", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "fix":
		if r.Method != http.MethodPost || !s.adminAuth(w, r) {
			return
		}
		if err := s.life.FixWithAO(r.Context(), key); err != nil {
			http.Error(w, "fix unavailable", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "retry":
		// Retry authorizes exactly one further dispatch, and only for a send
		// that failed. It keeps the original attempt in the audit trail.
		if r.Method != http.MethodPost || !s.adminAuth(w, r) {
			return
		}
		actor, ok := decodeActor(w, r)
		if !ok {
			return
		}
		// The preconditions are checked before the authorization is committed.
		// Spending the one-shot retry on a dispatch that cannot happen — the
		// kill switch is on, the session is gone — would leave the trigger with
		// no attempt sent and no authorization left.
		if err := s.life.CheckDispatchable(r.Context(), key); err != nil {
			http.Error(w, "retry unavailable", http.StatusConflict)
			return
		}
		if err := s.ledger.AuthorizeSendRetry(r.Context(), key, actor, s.now()); err != nil {
			http.Error(w, "retry unavailable", http.StatusConflict)
			return
		}
		if err := s.life.FixWithAO(r.Context(), key); err != nil {
			http.Error(w, "fix unavailable", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) automation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !s.adminAuth(w, r) {
		return
	}
	var body struct {
		Disabled bool   `json:"disabled"`
		Actor    string `json:"actor"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !validActor(body.Actor) {
		http.Error(w, "invalid actor", http.StatusBadRequest)
		return
	}
	if err := s.ledger.SetAutomationDisabled(r.Context(), body.Disabled, body.Actor, s.now()); err != nil {
		http.Error(w, "unable to update automation", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminAuth(w http.ResponseWriter, r *http.Request) bool {
	value := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(value, prefix)), s.admin) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) callbackAuth(r *http.Request, key domain.TriggerKey) bool {
	value := r.Header.Get("Authorization")
	return strings.HasPrefix(value, "Bearer ") && s.life.VerifyCallbackToken(key, strings.TrimPrefix(value, "Bearer "))
}

func decodeActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Actor string `json:"actor"`
	}
	if !decodeBody(w, r, &body) {
		return "", false
	}
	if !validActor(body.Actor) {
		http.Error(w, "invalid actor", http.StatusBadRequest)
		return "", false
	}
	return body.Actor, true
}

func validActor(actor string) bool {
	return len(actor) <= 128 && strings.TrimSpace(actor) != ""
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
	}
	return body, err
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := readBody(w, r, 4096)
	if err != nil {
		return false
	}
	if json.Unmarshal(body, target) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(value)
}
