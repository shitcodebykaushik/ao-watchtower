package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shitcodebykaushik/ao-watchtower/internal/ao"
	"github.com/shitcodebykaushik/ao-watchtower/internal/config"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/intake"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
)

func TestHelpDocumentsOneCommandPath(t *testing.T) {
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "watchtower up") {
		t.Fatalf("help=%q", output.String())
	}
}

func TestExplicitAOExecutableIsPreserved(t *testing.T) {
	const executable = "/custom/ao"
	got, err := resolveAOExecutable(executable)
	if err != nil || got != executable {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

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
