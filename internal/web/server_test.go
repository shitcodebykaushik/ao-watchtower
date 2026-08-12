package web

import (
	"bytes"
	"context"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ao"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
	"github.com/shitcodebykaushik/ao-watchtower/internal/service"
	"html/template"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fake struct{}

func (fake) SpawnInvestigatorSession(context.Context, ao.InvestigatorRequest) (ao.Session, error) {
	return ao.Session{ID: "s", ProjectID: "p"}, nil
}
func (fake) SendApprovedFollowup(context.Context, string, string) (ao.CommandResult, error) {
	return ao.CommandResult{}, nil
}
func setup(t *testing.T) (http.Handler, *service.Lifecycle, domain.TriggerKey) {
	l, e := ledger.Open(filepath.Join(t.TempDir(), "x.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	r, _ := domain.ParseRepository("o/r")
	f := domain.CheckSuiteFacts{ProviderID: 1, Repository: r, PullNumber: 2, HeadSHA: "abcdef0123456789", Conclusion: "failure"}
	k, _ := domain.NewCIFailureTriggerKey(r, 2, f.HeadSHA)
	res, e := l.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: "d", PayloadDigest: "x", ReceivedAt: time.Now()}, f, policy.Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: policy.OutcomeReserved, TriggerKey: k, AOProjectID: "p"})
	if e != nil {
		t.Fatal(e)
	}
	life, e := service.NewLifecycle(l, fake{}, service.Options{CallbackBaseURL: "http://x", CallbackSecret: []byte("callback")})
	if e != nil {
		t.Fatal(e)
	}
	if e = life.ProcessReservation(context.Background(), res, f); e != nil {
		t.Fatal(e)
	}
	s, e := New(l, life, "admin", true)
	if e != nil {
		t.Fatal(e)
	}
	return s.Handler(), life, k
}
func TestDashboardControlsAndAdminActions(t *testing.T) {
	h, life, k := setup(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "DEMO MODE") || !strings.Contains(body, "demo-admin-token") || !strings.Contains(body, "Fix with AO") || !strings.Contains(body, "Disable automation") {
		t.Fatalf("dashboard=%d %s", w.Code, body)
	}
	// A diagnosis callback credential cannot authorize a human approval.
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/triggers?action=approve&trigger="+string(k), strings.NewReader(`{"actor":"me"}`))
	r.Header.Set("Authorization", "Bearer "+life.CallbackToken(k))
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("callback approval=%d", w.Code)
	}
	// Submit a valid diagnosis with its scoped callback, then use the admin token for approval and fix.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/triggers?action=diagnosis&trigger="+string(k), strings.NewReader(`{"category":"code","confidence":0.5,"summary":"broken","evidence":[{"file":"x"}],"recommendedAction":"fix_code"}`))
	r.Header.Set("Authorization", "Bearer "+life.CallbackToken(k))
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("diagnosis=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/triggers?action=approve&trigger="+string(k), strings.NewReader(`{"actor":"me"}`))
	r.Header.Set("Authorization", "Bearer admin")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("approve=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/triggers?action=fix&trigger="+string(k), nil)
	r.Header.Set("Authorization", "Bearer admin")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("fix=%d", w.Code)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/automation", strings.NewReader(`{"disabled":true,"actor":"me"}`))
	r.Header.Set("Authorization", "Bearer admin")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("automation=%d", w.Code)
	}
}
func TestCallbackTokenIsScoped(t *testing.T) {
	_, l, k := setup(t)
	if !l.VerifyCallbackToken(k, l.CallbackToken(k)) || l.VerifyCallbackToken(k+"x", l.CallbackToken(k)) {
		t.Fatal("callback scope failed")
	}
}

func TestDashboardOmitsActionsForEmptyTrigger(t *testing.T) {
	template, err := template.New("dashboard").Parse(page)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = template.Execute(&out, struct {
		ledger.Dashboard
		Demo bool
	}{Dashboard: ledger.Dashboard{Rows: []ledger.DashboardRow{{Repository: "o/r", Evaluation: "non_failure"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "data-trigger") {
		t.Fatal("empty trigger rendered an action button")
	}
}
