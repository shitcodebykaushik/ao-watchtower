package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/ao"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
)

type fakeAO struct {
	spawnCalls int
	sendCalls  int
	spawn      func(context.Context, ao.InvestigatorRequest) (ao.Session, error)
	send       func(context.Context, string, string) (ao.CommandResult, error)
}

func (f *fakeAO) SpawnInvestigatorSession(ctx context.Context, request ao.InvestigatorRequest) (ao.Session, error) {
	f.spawnCalls++
	if f.spawn != nil {
		return f.spawn(ctx, request)
	}
	return ao.Session{ID: "session-1", ProjectID: request.ProjectID, Status: "running"}, nil
}
func (f *fakeAO) SendApprovedFollowup(ctx context.Context, sessionID, message string) (ao.CommandResult, error) {
	f.sendCalls++
	if f.send != nil {
		return f.send(ctx, sessionID, message)
	}
	return ao.CommandResult{}, nil
}

func reserve(t *testing.T, l *ledger.Ledger) (ledger.Result, domain.CheckSuiteFacts) {
	t.Helper()
	repository, _ := domain.ParseRepository("octo/repo")
	facts := domain.CheckSuiteFacts{ProviderID: 1, Repository: repository, PullNumber: 4, HeadSHA: "abcdef0123456789", Conclusion: "failure"}
	key, _ := domain.NewCIFailureTriggerKey(repository, facts.PullNumber, facts.HeadSHA)
	result, err := l.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: "delivery-" + time.Now().Format("150405.000000000"), PayloadDigest: "digest", ReceivedAt: time.Now()}, facts, policy.Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: policy.OutcomeReserved, TriggerKey: key, AOProjectID: "project-1"})
	if err != nil || !result.Reserved {
		t.Fatalf("reserve result=%#v err=%v", result, err)
	}
	return result, facts
}

func newLifecycle(t *testing.T) (*ledger.Ledger, *Lifecycle, *fakeAO) {
	t.Helper()
	l, err := ledger.Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	fake := &fakeAO{}
	s, err := NewLifecycle(l, fake)
	if err != nil {
		t.Fatal(err)
	}
	return l, s, fake
}

func validDiagnosis() []byte {
	return []byte(`{"category":"code","confidence":0.9,"summary":"parser rejects a valid request","evidence":[{"file":"internal/parser.go","line":8,"check":"TestParser"}],"recommendedAction":"fix_code"}`)
}

func TestProcessReservationSpawnsOnlyOnce(t *testing.T) {
	l, service, fake := newLifecycle(t)
	reservation, facts := reserve(t, l)
	if err := service.ProcessReservation(context.Background(), reservation, facts); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessReservation(context.Background(), reservation, facts); err != nil {
		t.Fatal(err)
	}
	if fake.spawnCalls != 1 {
		t.Fatalf("spawn calls=%d, want 1", fake.spawnCalls)
	}
	if count, _ := l.Count(context.Background(), "spawn_attempts"); count != 1 {
		t.Fatalf("spawn attempts=%d", count)
	}
}

func TestProcessReservationKillSwitchBlocksSpawn(t *testing.T) {
	l, service, fake := newLifecycle(t)
	reservation, facts := reserve(t, l)
	if err := l.SetAutomationDisabled(context.Background(), true, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessReservation(context.Background(), reservation, facts); err != nil {
		t.Fatal(err)
	}
	if fake.spawnCalls != 0 {
		t.Fatalf("spawn calls=%d", fake.spawnCalls)
	}
	if count, _ := l.Count(context.Background(), "spawn_attempts"); count != 1 {
		t.Fatalf("blocked attempt was not audited: %d", count)
	}
}

func TestProcessReservationAuditsFailureAndClaimConflict(t *testing.T) {
	for name, testCase := range map[string]struct {
		err     error
		outcome string
	}{"failed": {errors.New("AO unavailable"), "failed"}, "claim conflict": {&ao.ClaimConflictError{Cause: errors.New("conflict")}, "claim_conflict"}} {
		t.Run(name, func(t *testing.T) {
			l, service, fake := newLifecycle(t)
			reservation, facts := reserve(t, l)
			fake.spawn = func(context.Context, ao.InvestigatorRequest) (ao.Session, error) { return ao.Session{}, testCase.err }
			if err := service.ProcessReservation(context.Background(), reservation, facts); err != nil {
				t.Fatal(err)
			}
			if fake.spawnCalls != 1 {
				t.Fatalf("spawn calls=%d", fake.spawnCalls)
			}
			attempt, found, err := l.SpawnAttemptForTrigger(context.Background(), reservation.TriggerKey)
			if err != nil || !found || attempt.Outcome != testCase.outcome {
				t.Fatalf("attempt=%#v found=%v err=%v", attempt, found, err)
			}
			if _, found, err := l.SessionForTrigger(context.Background(), reservation.TriggerKey); err != nil || found {
				t.Fatalf("session found=%v err=%v", found, err)
			}
			if err := service.ProcessReservation(context.Background(), reservation, facts); err != nil {
				t.Fatal(err)
			}
			if fake.spawnCalls != 1 {
				t.Fatalf("claim/failure retry spawned again: %d", fake.spawnCalls)
			}
		})
	}
}

func TestInvalidDiagnosisIsRetainedButUnusable(t *testing.T) {
	l, service, _ := newLifecycle(t)
	reservation, _ := reserve(t, l)
	valid, err := service.SubmitDiagnosis(context.Background(), reservation.TriggerKey, []byte(`{"category":"not-allowed"}`))
	if err != nil || valid {
		t.Fatalf("valid=%v err=%v", valid, err)
	}
	record, found, err := l.LatestDiagnosisRecord(context.Background(), reservation.TriggerKey)
	if err != nil || !found || record.Valid || string(record.Raw) != `{"category":"not-allowed"}` {
		t.Fatalf("record=%#v found=%v err=%v", record, found, err)
	}
	if err := service.ApproveFix(context.Background(), reservation.TriggerKey, "human"); !errors.Is(err, ErrNoValidDiagnosis) {
		t.Fatalf("approval error=%v", err)
	}
}

func TestFixRequiresApprovalAndRechecksKillSwitch(t *testing.T) {
	l, service, fake := newLifecycle(t)
	reservation, facts := reserve(t, l)
	if err := service.ProcessReservation(context.Background(), reservation, facts); err != nil {
		t.Fatal(err)
	}
	if valid, err := service.SubmitDiagnosis(context.Background(), reservation.TriggerKey, validDiagnosis()); err != nil || !valid {
		t.Fatalf("diagnosis valid=%v err=%v", valid, err)
	}
	if err := service.FixWithAO(context.Background(), reservation.TriggerKey); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("FixWithAO without approval = %v", err)
	}
	if fake.sendCalls != 0 {
		t.Fatalf("send calls=%d", fake.sendCalls)
	}
	if err := service.ApproveFix(context.Background(), reservation.TriggerKey, "human"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAutomationDisabled(context.Background(), true, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := service.FixWithAO(context.Background(), reservation.TriggerKey); !errors.Is(err, ErrAutomationDisabled) {
		t.Fatalf("FixWithAO kill-switch error = %v", err)
	}
	if fake.sendCalls != 0 {
		t.Fatalf("kill switch allowed send")
	}
	if count, _ := l.Count(context.Background(), "send_attempts"); count != 1 {
		t.Fatalf("send attempt count=%d", count)
	}
}

func TestFixWithAORecordsApprovedSend(t *testing.T) {
	l, service, fake := newLifecycle(t)
	reservation, facts := reserve(t, l)
	if err := service.ProcessReservation(context.Background(), reservation, facts); err != nil {
		t.Fatal(err)
	}
	if valid, err := service.SubmitDiagnosis(context.Background(), reservation.TriggerKey, validDiagnosis()); err != nil || !valid {
		t.Fatalf("diagnosis valid=%v err=%v", valid, err)
	}
	if err := service.ApproveFix(context.Background(), reservation.TriggerKey, "human"); err != nil {
		t.Fatal(err)
	}
	if err := service.FixWithAO(context.Background(), reservation.TriggerKey); err != nil {
		t.Fatal(err)
	}
	if fake.sendCalls != 1 {
		t.Fatalf("send calls=%d", fake.sendCalls)
	}
	if count, _ := l.Count(context.Background(), "send_attempts"); count != 1 {
		t.Fatalf("send attempts=%d", count)
	}
}

func TestSubmitDiagnosisRejectsTrailingMalformedData(t *testing.T) {
	l, service, _ := newLifecycle(t)
	reservation, _ := reserve(t, l)
	valid, err := service.SubmitDiagnosis(context.Background(), reservation.TriggerKey, append(validDiagnosis(), []byte(` trailing`)...))
	if err != nil || valid {
		t.Fatalf("valid=%v err=%v", valid, err)
	}
	record, found, err := l.LatestDiagnosisRecord(context.Background(), reservation.TriggerKey)
	if err != nil || !found || record.Valid {
		t.Fatalf("record=%#v found=%v err=%v", record, found, err)
	}
}

func TestCallbackTokenAndPromptAreScoped(t *testing.T) {
	l, _, _ := newLifecycle(t)
	fake := &fakeAO{}
	s, err := NewLifecycle(l, fake, Options{CallbackBaseURL: "https://watchtower.example", CallbackSecret: []byte("callback")})
	if err != nil {
		t.Fatal(err)
	}
	_, facts := reserve(t, l)
	key, _ := domain.NewCIFailureTriggerKey(facts.Repository, facts.PullNumber, facts.HeadSHA)
	token := s.CallbackToken(key)
	if token == "" || !s.VerifyCallbackToken(key, token) || s.VerifyCallbackToken(key+"x", token) {
		t.Fatal("callback token scope invalid")
	}
	if prompt := s.investigatorPrompt(facts, key); !strings.Contains(prompt, "action=diagnosis") || !strings.Contains(prompt, token) {
		t.Fatal("callback instruction missing")
	}
}
