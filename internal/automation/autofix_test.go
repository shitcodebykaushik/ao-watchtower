package automation

import (
	"context"
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
