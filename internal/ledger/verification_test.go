package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

// reserveTrigger drives a delivery through the ledger and returns the reserved
// trigger key, so verification tests start from real durable facts.
func reserveTrigger(t *testing.T, l *Ledger, at time.Time) domain.TriggerKey {
	t.Helper()
	facts, evaluation := testFacts(t), testEval(t)
	result, err := l.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: "delivery", PayloadDigest: "digest", ReceivedAt: at}, facts, evaluation)
	if err != nil || !result.Reserved {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	return result.TriggerKey
}

// dispatchFix walks a trigger through spawn, diagnosis, approval, and send so a
// verification exists to assert against.
func dispatchFix(t *testing.T, l *Ledger, key domain.TriggerKey, at time.Time) {
	t.Helper()
	ctx := context.Background()
	start, err := l.StartSpawnAttempt(ctx, key, "project", at)
	if err != nil || !start.Started {
		t.Fatalf("spawn start=%#v err=%v", start, err)
	}
	if err := l.CompleteSpawnAttempt(ctx, key, "spawned", "session-1", "", at); err != nil {
		t.Fatal(err)
	}
	diagnosis := domain.Diagnosis{Category: "code", Confidence: 0.95, Summary: "regression in Add", Evidence: []domain.DiagnosisEvidence{{File: "calculator.go", Line: 4, Check: "TestAdd"}}, RecommendedAction: "fix_code"}
	if err := l.RecordDiagnosis(ctx, key, []byte(`{}`), &diagnosis, true, at); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordHumanApproval(ctx, key, "operator", at); err != nil {
		t.Fatal(err)
	}
	send, err := l.StartSendAttempt(ctx, key, "session-1", at)
	if err != nil || !send.Started {
		t.Fatalf("send start=%#v err=%v", send, err)
	}
}

func TestSuccessfulSendOpensExactlyOneVerification(t *testing.T) {
	l := newLedger(t)
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, l, at)
	dispatchFix(t, l, key, at)
	if err := l.CompleteSendAttempt(context.Background(), key, "session-1", "sent", "", at); err != nil {
		t.Fatal(err)
	}
	pending, err := l.PendingVerifications(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	entry := pending[0]
	if entry.TriggerKey != key || entry.PullNumber != 2 || entry.DispatchedHeadSHA != "abcdef0123456789" || entry.Repository.String() != "octo/repo" {
		t.Fatalf("entry=%#v", entry)
	}
	if !entry.StartedAt.Equal(at) {
		t.Fatalf("startedAt=%s", entry.StartedAt)
	}
}

func TestFailedSendDoesNotOpenVerification(t *testing.T) {
	l := newLedger(t)
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, l, at)
	dispatchFix(t, l, key, at)
	if err := l.CompleteSendAttempt(context.Background(), key, "session-1", "failed", "AO unreachable", at); err != nil {
		t.Fatal(err)
	}
	pending, err := l.PendingVerifications(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
}

func TestResolveVerificationIsTerminalAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, l, at)
	dispatchFix(t, l, key, at)
	ctx := context.Background()
	if err := l.CompleteSendAttempt(ctx, key, "session-1", "sent", "", at); err != nil {
		t.Fatal(err)
	}
	greenAt := at.Add(4 * time.Minute)
	if err := l.ResolveVerification(ctx, key, "0123456789ABCDEF", VerificationGreen, "CI passed", greenAt); err != nil {
		t.Fatal(err)
	}
	// A replayed observation must not rewrite a settled fact.
	if err := l.ResolveVerification(ctx, key, "deadbeef1234567", VerificationStillFailing, "late replay", greenAt.Add(time.Minute)); err != nil {
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
	outcome, found, err := l.VerificationOutcome(ctx, key)
	if err != nil || !found || outcome != VerificationGreen {
		t.Fatalf("outcome=%s found=%t err=%v", outcome, found, err)
	}
	pending, err := l.PendingVerifications(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	stats, err := l.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.VerifiedGreen != 1 || stats.StillFailing != 0 || stats.Dispatched != 1 || stats.Approvals != 1 || stats.ValidDiagnoses != 1 || stats.Spawned != 1 {
		t.Fatalf("stats=%#v", stats)
	}
	if stats.MedianTimeToGreen != 4*time.Minute {
		t.Fatalf("medianTimeToGreen=%s", stats.MedianTimeToGreen)
	}
	if stats.RepairSuccessRate() != 1 || stats.AutoFixRate() != 1 {
		t.Fatalf("rates=%f/%f", stats.RepairSuccessRate(), stats.AutoFixRate())
	}
	if err := l.ResolveVerification(ctx, key, "", "not_an_outcome", "", greenAt); err == nil {
		t.Fatal("expected an unsupported verification outcome to be rejected")
	}
}

// An investigator that crashes before submitting a diagnosis must not hold a
// concurrency slot forever, or enough of them would block every future
// investigation permanently.
func TestStalledInvestigationsStopConsumingCapacity(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, l, at)
	start, err := l.StartSpawnAttempt(ctx, key, "project", at)
	if err != nil || !start.Started {
		t.Fatalf("start=%#v err=%v", start, err)
	}
	if err := l.CompleteSpawnAttempt(ctx, key, "spawned", "session-1", "", at); err != nil {
		t.Fatal(err)
	}
	active, err := l.ActiveInvestigations(ctx, at.Add(time.Minute), 30*time.Minute)
	if err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	active, err = l.ActiveInvestigations(ctx, at.Add(31*time.Minute), 30*time.Minute)
	if err != nil || active != 0 {
		t.Fatalf("stale investigation still counted: active=%d err=%v", active, err)
	}
	// A diagnosis releases the slot regardless of age.
	diagnosis := domain.Diagnosis{Category: "code", Confidence: 0.9, Summary: "found it", Evidence: []domain.DiagnosisEvidence{{Check: "TestAdd"}}, RecommendedAction: "fix_code"}
	if err := l.RecordDiagnosis(ctx, key, []byte(`{}`), &diagnosis, true, at); err != nil {
		t.Fatal(err)
	}
	active, err = l.ActiveInvestigations(ctx, at.Add(time.Minute), 30*time.Minute)
	if err != nil || active != 0 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	if _, err := l.ActiveInvestigations(ctx, time.Time{}, time.Minute); err == nil {
		t.Fatal("expected a zero time to be rejected")
	}
	if _, err := l.ActiveInvestigations(ctx, at, 0); err == nil {
		t.Fatal("expected a non-positive stale threshold to be rejected")
	}
}

// A repository with two workflows produces two deliveries for one trigger. That
// is one repair and must contribute one sample to the median, not two.
func TestMedianTimeToGreenCountsEachRepairOnce(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	facts, evaluation := testFacts(t), testEval(t)
	for index, delivery := range []struct {
		id       string
		digest   string
		received time.Time
	}{
		{"delivery-build", "digest-build", at},
		{"delivery-lint", "digest-lint", at.Add(2 * time.Minute)},
	} {
		suiteFacts := facts
		suiteFacts.ProviderID = int64(index + 1)
		result, err := l.RecordEvaluation(ctx, domain.WebhookDelivery{ID: delivery.id, PayloadDigest: delivery.digest, ReceivedAt: delivery.received}, suiteFacts, evaluation)
		if err != nil {
			t.Fatal(err)
		}
		if result.TriggerKey != evaluation.TriggerKey {
			t.Fatalf("expected both deliveries to share one trigger, got %s", result.TriggerKey)
		}
	}
	key := evaluation.TriggerKey
	dispatchFix(t, l, key, at)
	if err := l.CompleteSendAttempt(ctx, key, "session-1", "sent", "", at); err != nil {
		t.Fatal(err)
	}
	if err := l.ResolveVerification(ctx, key, "0123456789abcdef", VerificationGreen, "CI passed", at.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	stats, err := l.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Measured from the earliest delivery for the trigger, counted once. Two
	// samples would average to 9m instead.
	if stats.MedianTimeToGreen != 10*time.Minute {
		t.Fatalf("medianTimeToGreen=%s want 10m0s", stats.MedianTimeToGreen)
	}
	if stats.VerifiedGreen != 1 {
		t.Fatalf("verifiedGreen=%d", stats.VerifiedGreen)
	}
}

// `watchtower stats` opens the same file a running monitor is writing to.
func TestConcurrentReaderDoesNotBlockWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, writer, at)
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("a second reader must be able to open the ledger: %v", err)
	}
	defer reader.Close()
	if _, err := reader.Stats(ctx); err != nil {
		t.Fatalf("concurrent read failed: %v", err)
	}
	if err := writer.RecordHumanApproval(ctx, key, "operator", at); err != nil {
		t.Fatalf("concurrent write failed: %v", err)
	}
	if _, err := reader.Stats(ctx); err != nil {
		t.Fatalf("read after a concurrent write failed: %v", err)
	}
}

func TestSendRetryRequiresAFailedDispatch(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, l, at)
	if err := l.AuthorizeSendRetry(ctx, key, "operator", at); err == nil {
		t.Fatal("expected a retry without any dispatch to be rejected")
	}
	dispatchFix(t, l, key, at)
	if err := l.CompleteSendAttempt(ctx, key, "session-1", "sent", "", at); err != nil {
		t.Fatal(err)
	}
	if err := l.AuthorizeSendRetry(ctx, key, "operator", at); err == nil {
		t.Fatal("expected a successful dispatch to refuse a retry")
	}
	// A second attempt is still blocked while the first one succeeded.
	start, err := l.StartSendAttempt(ctx, key, "session-1", at)
	if err != nil || start.Started || start.Blocked {
		t.Fatalf("start=%#v err=%v", start, err)
	}
}

func TestAuthorizedRetryUnblocksExactlyOneFurtherDispatch(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, l, at)
	dispatchFix(t, l, key, at)
	if err := l.CompleteSendAttempt(ctx, key, "session-1", "failed", "AO unreachable", at); err != nil {
		t.Fatal(err)
	}
	blocked, err := l.StartSendAttempt(ctx, key, "session-1", at)
	if err != nil || blocked.Started {
		t.Fatalf("a failed dispatch must stay blocked until a retry is authorized: %#v err=%v", blocked, err)
	}
	if err := l.AuthorizeSendRetry(ctx, key, "operator", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	retried, err := l.StartSendAttempt(ctx, key, "session-1", at.Add(2*time.Minute))
	if err != nil || !retried.Started {
		t.Fatalf("retried=%#v err=%v", retried, err)
	}
	// The authorization is spent: without a new one the next attempt blocks again.
	again, err := l.StartSendAttempt(ctx, key, "session-1", at.Add(3*time.Minute))
	if err != nil || again.Started {
		t.Fatalf("again=%#v err=%v", again, err)
	}
	if err := l.CompleteSendAttempt(ctx, key, "session-1", "sent", "", at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Both attempts stay in the audit trail.
	attempts, err := l.Count(ctx, "send_attempts")
	if err != nil || attempts != 2 {
		t.Fatalf("send attempts=%d err=%v", attempts, err)
	}
}

func TestKillSwitchStillBlocksAfterARetryAuthorization(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := reserveTrigger(t, l, at)
	dispatchFix(t, l, key, at)
	if err := l.CompleteSendAttempt(ctx, key, "session-1", "failed", "AO unreachable", at); err != nil {
		t.Fatal(err)
	}
	if err := l.AuthorizeSendRetry(ctx, key, "operator", at); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAutomationDisabled(ctx, true, "operator", at); err != nil {
		t.Fatal(err)
	}
	start, err := l.StartSendAttempt(ctx, key, "session-1", at)
	if err != nil || start.Started || !start.Blocked {
		t.Fatalf("start=%#v err=%v", start, err)
	}
}
