package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/agent-orchestrator/ao-watchtower/internal/ao"
	"github.com/agent-orchestrator/ao-watchtower/internal/config"
	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
	"github.com/agent-orchestrator/ao-watchtower/internal/intake"
	"github.com/agent-orchestrator/ao-watchtower/internal/ledger"
)

func TestDemoUsesExplicitFakeBoundary(t *testing.T) {
	got, e := demoAO{}.SpawnInvestigatorSession(context.Background(), ao.InvestigatorRequest{ProjectID: "p"})
	if e != nil || got.ID != "demo-session" || got.ProjectID != "p" {
		t.Fatalf("demo=%#v %v", got, e)
	}
}
func TestDemoRequestPassesRealSignedIntake(t *testing.T) {
	l, e := ledger.Open(filepath.Join(t.TempDir(), "d.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	repo, _ := domain.ParseRepository("demo/repo")
	c := config.Defaults()
	c.RepositoryProjects = []domain.RepositoryProject{{Repository: repo, AOProjectID: "demo-project"}}
	h, e := intake.NewHandler([]byte("demo-webhook-secret"), c, l)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newDemoRequest("http://example/webhooks/github", "demo-webhook-secret"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d", w.Code)
	}
	n, e := l.Count(context.Background(), "webhook_deliveries")
	if e != nil || n != 1 {
		t.Fatalf("deliveries=%d err=%v", n, e)
	}
}
