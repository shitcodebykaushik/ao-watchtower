package ledger

import (
	"context"
	"fmt"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newLedger(t *testing.T) *Ledger {
	t.Helper()
	l, e := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	return l
}
func testFacts(t *testing.T) domain.CheckSuiteFacts {
	t.Helper()
	r, e := domain.ParseRepository("octo/repo")
	if e != nil {
		t.Fatal(e)
	}
	return domain.CheckSuiteFacts{ProviderID: 8, Repository: r, PullNumber: 2, HeadSHA: "abcdef0123456789", Conclusion: "failure"}
}
func testEval(t *testing.T) policy.Evaluation {
	f := testFacts(t)
	k, e := domain.NewCIFailureTriggerKey(f.Repository, f.PullNumber, f.HeadSHA)
	if e != nil {
		t.Fatal(e)
	}
	return policy.Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: policy.OutcomeReserved, TriggerKey: k, AOProjectID: "project"}
}
func TestRecordEvaluationDuplicateAndConcurrentReservation(t *testing.T) {
	l := newLedger(t)
	f, e := testFacts(t), testEval(t)
	at := time.Now()
	d := domain.WebhookDelivery{ID: "delivery-1", PayloadDigest: "digest", ReceivedAt: at}
	first, err := l.RecordEvaluation(context.Background(), d, f, e)
	if err != nil || !first.Reserved {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := l.RecordEvaluation(context.Background(), d, f, e)
	if err != nil || second.Reserved {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	var wg sync.WaitGroup
	results := make(chan Result, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := l.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: fmt.Sprintf("delivery-%d", i+2), PayloadDigest: fmt.Sprintf("digest-%d", i), ReceivedAt: at}, f, e)
			results <- result
			errs <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for result := range results {
		if result.Reserved {
			count++
		}
	}
	if count != 0 {
		t.Fatalf("concurrent reservations=%d want 0", count)
	}
}

func TestLifecycleFactsSurviveReopenAndBoundRawDiagnosis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	facts, evaluation := testFacts(t), testEval(t)
	result, err := l.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: "delivery", PayloadDigest: "digest", ReceivedAt: time.Now()}, facts, evaluation)
	if err != nil || !result.Reserved {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if err := l.SetAutomationDisabled(context.Background(), true, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordDiagnosis(context.Background(), result.TriggerKey, make([]byte, MaxDiagnosisRaw+1), nil, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	disabled, err := l.AutomationDisabled(context.Background())
	if err != nil || !disabled {
		t.Fatalf("disabled=%v err=%v", disabled, err)
	}
	record, found, err := l.LatestDiagnosisRecord(context.Background(), result.TriggerKey)
	if err != nil || !found || record.Valid || len(record.Raw) != MaxDiagnosisRaw {
		t.Fatalf("record raw=%d valid=%v found=%v err=%v", len(record.Raw), record.Valid, found, err)
	}
}

func TestDashboardIncludesEvaluationWithoutTrigger(t *testing.T) {
	l := newLedger(t)
	facts := testFacts(t)
	facts.Conclusion = "success"
	_, err := l.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: "success", PayloadDigest: "digest", ReceivedAt: time.Now()}, facts, policy.Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: policy.OutcomeNonFailure})
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := l.Dashboard(context.Background())
	if err != nil || len(dashboard.Rows) != 1 || dashboard.Rows[0].TriggerKey != "" || dashboard.Rows[0].Evaluation != policy.OutcomeNonFailure {
		t.Fatalf("dashboard=%#v err=%v", dashboard, err)
	}
}
