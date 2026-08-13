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
	"github.com/shitcodebykaushik/ao-watchtower/internal/repopolicy"
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

func TestVersionCommandReportsBuildIdentity(t *testing.T) {
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"version"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), Version) {
		t.Fatalf("version=%q", output.String())
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"teleport"}, &output); err == nil {
		t.Fatal("expected an unknown command to be rejected")
	}
}

func TestPolicyGateRefusesADeniedPath(t *testing.T) {
	policy, err := repopolicy.Parse([]byte(`{"version":1,"autoFix":{"deniedPaths":[".github/**"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	gate := policyGate{policy: policy, floor: 0.8}
	denied := domain.Diagnosis{Category: "code", Confidence: 0.99, Summary: "workflow broken", Evidence: []domain.DiagnosisEvidence{{File: ".github/workflows/ci.yml", Check: "build"}}, RecommendedAction: "fix_code"}
	if allowed, reason := gate.AllowAutoFix(denied); allowed || reason == "" {
		t.Fatalf("allowed=%t reason=%q", allowed, reason)
	}
	permitted := domain.Diagnosis{Category: "code", Confidence: 0.99, Summary: "regression", Evidence: []domain.DiagnosisEvidence{{File: "internal/calculator.go", Check: "TestAdd"}}, RecommendedAction: "fix_code"}
	if allowed, _ := gate.AllowAutoFix(permitted); !allowed {
		t.Fatal("expected an allowed path to pass the gate")
	}
}

func TestLimitLabelDescribesUnlimited(t *testing.T) {
	if limitLabel(0) != "unlimited" || limitLabel(3) != "3" {
		t.Fatalf("labels=%q/%q", limitLabel(0), limitLabel(3))
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
