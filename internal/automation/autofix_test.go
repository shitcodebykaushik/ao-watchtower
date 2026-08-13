package automation

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/config"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
)

type fakeLifecycle struct{ approvals, fixes int }

func (f *fakeLifecycle) ApproveFix(context.Context, domain.TriggerKey, string) error {
	f.approvals++
	return nil
}
func (f *fakeLifecycle) FixWithAO(context.Context, domain.TriggerKey) error { f.fixes++; return nil }

func TestControllerDispatchesOnlyEligibleDiagnosisOnce(t *testing.T) {
	durable, err := ledger.Open(filepath.Join(t.TempDir(), "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	repository, _ := domain.ParseRepository("acme/app")
	configuration := config.Defaults()
	configuration.RepositoryProjects = []domain.RepositoryProject{{Repository: repository, AOProjectID: "app"}}
	facts := domain.CheckSuiteFacts{ProviderID: 1, Repository: repository, PullNumber: 7, HeadSHA: "abcdef0123456789", Conclusion: "failure"}
	evaluation, _ := policy.EvaluateCheckSuite(facts, configuration)
	result, err := durable.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: "delivery", PayloadDigest: "digest", ReceivedAt: time.Now()}, facts, evaluation)
	if err != nil {
		t.Fatal(err)
	}
	start, err := durable.StartSpawnAttempt(context.Background(), result.TriggerKey, "app", time.Now())
	if err != nil || !start.Started {
		t.Fatalf("start=%#v err=%v", start, err)
	}
	if err := durable.CompleteSpawnAttempt(context.Background(), result.TriggerKey, "spawned", "app-1", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	diagnosis := domain.Diagnosis{Category: "code", Confidence: .95, Summary: "broken", Evidence: []domain.DiagnosisEvidence{{File: "x.go", Line: 1, Check: "TestX"}}, RecommendedAction: "fix_code"}
	if err := durable.RecordDiagnosis(context.Background(), result.TriggerKey, []byte(`{}`), &diagnosis, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	lifecycle := &fakeLifecycle{}
	controller, err := New(durable, lifecycle, "auto-fix", DefaultMinimumConfidence, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lifecycle.approvals != 1 || lifecycle.fixes != 1 {
		t.Fatalf("approvals=%d fixes=%d", lifecycle.approvals, lifecycle.fixes)
	}
}

// diagnosedTrigger seeds a ledger with one spawned, validly diagnosed trigger
// so guardrail tests start from the exact state auto-fix acts on.
func diagnosedTrigger(t *testing.T, durable *ledger.Ledger, pullNumber int64, at time.Time) domain.TriggerKey {
	t.Helper()
	repository, _ := domain.ParseRepository("acme/app")
	configuration := config.Defaults()
	configuration.RepositoryProjects = []domain.RepositoryProject{{Repository: repository, AOProjectID: "app"}}
	facts := domain.CheckSuiteFacts{ProviderID: pullNumber, Repository: repository, PullNumber: pullNumber, HeadSHA: "abcdef012345678", Conclusion: "failure"}
	evaluation, err := policy.EvaluateCheckSuite(facts, configuration)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	result, err := durable.RecordEvaluation(ctx, domain.WebhookDelivery{ID: fmt.Sprintf("delivery-%d", pullNumber), PayloadDigest: fmt.Sprintf("digest-%d", pullNumber), ReceivedAt: at}, facts, evaluation)
	if err != nil || !result.Reserved {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	start, err := durable.StartSpawnAttempt(ctx, result.TriggerKey, "app", at)
	if err != nil || !start.Started {
		t.Fatalf("start=%#v err=%v", start, err)
	}
	if err := durable.CompleteSpawnAttempt(ctx, result.TriggerKey, "spawned", fmt.Sprintf("session-%d", pullNumber), "", at); err != nil {
		t.Fatal(err)
	}
	diagnosis := domain.Diagnosis{Category: "code", Confidence: .95, Summary: "broken", Evidence: []domain.DiagnosisEvidence{{File: "internal/x.go", Line: 1, Check: "TestX"}}, RecommendedAction: "fix_code"}
	if err := durable.RecordDiagnosis(ctx, result.TriggerKey, []byte(`{}`), &diagnosis, true, at); err != nil {
		t.Fatal(err)
	}
	return result.TriggerKey
}

type refusingGate struct {
	reason string
	calls  int
}

func (g *refusingGate) AllowAutoFix(domain.Diagnosis) (bool, string) {
	g.calls++
	return false, g.reason
}

func TestRepositoryPolicyGateBlocksDispatch(t *testing.T) {
	durable, err := ledger.Open(filepath.Join(t.TempDir(), "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	diagnosedTrigger(t, durable, 7, at)
	lifecycle := &fakeLifecycle{}
	gate := &refusingGate{reason: "denied_path"}
	controller, err := New(durable, lifecycle, "auto-fix", DefaultMinimumConfidence, time.Second, discardLogger(), Options{Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := controller.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if lifecycle.approvals != 0 || lifecycle.fixes != 0 {
		t.Fatalf("a refused policy must dispatch nothing: approvals=%d fixes=%d", lifecycle.approvals, lifecycle.fixes)
	}
	if gate.calls != 3 {
		t.Fatalf("gate calls=%d", gate.calls)
	}
}

func TestDailyFixBudgetStopsDispatch(t *testing.T) {
	durable, err := ledger.Open(filepath.Join(t.TempDir(), "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	// One fix already reached AO inside the rolling window.
	spent := diagnosedTrigger(t, durable, 7, at)
	if err := durable.RecordHumanApproval(ctx, spent, "operator", at); err != nil {
		t.Fatal(err)
	}
	send, err := durable.StartSendAttempt(ctx, spent, "session-7", at)
	if err != nil || !send.Started {
		t.Fatalf("send=%#v err=%v", send, err)
	}
	if err := durable.CompleteSendAttempt(ctx, spent, "session-7", "sent", "", at); err != nil {
		t.Fatal(err)
	}
	diagnosedTrigger(t, durable, 8, at.Add(time.Minute))

	lifecycle := &fakeLifecycle{}
	controller, err := New(durable, lifecycle, "auto-fix", DefaultMinimumConfidence, time.Second, discardLogger(), Options{DailyFixBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	controller.now = func() time.Time { return at.Add(time.Hour) }
	if err := controller.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if lifecycle.fixes != 0 {
		t.Fatalf("budget spent, expected no dispatch: fixes=%d", lifecycle.fixes)
	}
	// Once the window has rolled past the earlier dispatch, budget frees up.
	controller.now = func() time.Time { return at.Add(25 * time.Hour) }
	if err := controller.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if lifecycle.fixes != 1 {
		t.Fatalf("expected one dispatch after the window rolled: fixes=%d", lifecycle.fixes)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	durable, err := ledger.Open(filepath.Join(t.TempDir(), "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	if _, err := New(durable, &fakeLifecycle{}, "auto-fix", .8, time.Second, nil, Options{}, Options{}); err == nil {
		t.Fatal("expected more than one option set to be rejected")
	}
	if _, err := New(durable, &fakeLifecycle{}, "auto-fix", .8, time.Second, nil, Options{DailyFixBudget: -1}); err == nil {
		t.Fatal("expected a negative budget to be rejected")
	}
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestEligibilityIsNarrow(t *testing.T) {
	base := domain.Diagnosis{Category: "code", Confidence: .9, Summary: "x", Evidence: []domain.DiagnosisEvidence{{Check: "TestX"}}, RecommendedAction: "fix_code"}
	if !eligible(base, true, .8) {
		t.Fatal("expected eligible")
	}
	base.Category = "infrastructure"
	if eligible(base, true, .8) {
		t.Fatal("infrastructure must not auto-fix")
	}
}
