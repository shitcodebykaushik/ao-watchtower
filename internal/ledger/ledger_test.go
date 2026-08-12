package ledger

import (
	"context"
	"fmt"
	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
	"github.com/agent-orchestrator/ao-watchtower/internal/policy"
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
