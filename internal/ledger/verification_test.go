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
