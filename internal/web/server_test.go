package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/ao"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
	"github.com/shitcodebykaushik/ao-watchtower/internal/service"
)

type fake struct{ sendErr error }

func (fake) SpawnInvestigatorSession(context.Context, ao.InvestigatorRequest) (ao.Session, error) {
	return ao.Session{ID: "s", ProjectID: "p"}, nil
}
func (f fake) SendApprovedFollowup(context.Context, string, string) (ao.CommandResult, error) {
	return ao.CommandResult{}, f.sendErr
}

func setup(t *testing.T) (http.Handler, *service.Lifecycle, *ledger.Ledger, domain.TriggerKey) {
	t.Helper()
	return setupWith(t, fake{})
}

func setupWith(t *testing.T, client service.AOClient) (http.Handler, *service.Lifecycle, *ledger.Ledger, domain.TriggerKey) {
	t.Helper()
	durable, err := ledger.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { durable.Close() })
	repository, _ := domain.ParseRepository("o/r")
	facts := domain.CheckSuiteFacts{ProviderID: 1, Repository: repository, PullNumber: 2, HeadSHA: "abcdef0123456789", Conclusion: "failure", DetailsURL: "https://github.com/o/r/actions/runs/1"}
	key, _ := domain.NewCIFailureTriggerKey(repository, 2, facts.HeadSHA)
	result, err := durable.RecordEvaluation(context.Background(), domain.WebhookDelivery{ID: "d", PayloadDigest: "x", ReceivedAt: time.Now()}, facts,
		policy.Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: policy.OutcomeReserved, TriggerKey: key, AOProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	life, err := service.NewLifecycle(durable, client, service.Options{CallbackBaseURL: "http://x", CallbackSecret: []byte("callback")})
	if err != nil {
		t.Fatal(err)
	}
	if err := life.ProcessReservation(context.Background(), result, facts); err != nil {
		t.Fatal(err)
	}
	server, err := New(durable, life, "admin", true)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler(), life, durable, key
}

func call(t *testing.T, handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var request *http.Request
	if reader != nil {
		request = httptest.NewRequest(method, target, reader)
	} else {
		request = httptest.NewRequest(method, target, nil)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func fetchState(t *testing.T, handler http.Handler) State {
	t.Helper()
	response := call(t, handler, http.MethodGet, "/api/state", "admin", "")
	if response.Code != http.StatusOK {
		t.Fatalf("state=%d", response.Code)
	}
	var payload State
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// The shell must not leak ledger contents to anything that can reach the port.
func TestShellCarriesNoLedgerContent(t *testing.T) {
	handler, _, _, key := setup(t)
	response := call(t, handler, http.MethodGet, "/", "", "")
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "AO Watchtower") {
		t.Fatalf("shell=%d", response.Code)
	}
	if strings.Contains(body, string(key)) || strings.Contains(body, "abcdef0123456789") {
		t.Fatal("the unauthenticated shell must not embed ledger facts")
	}
	if response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("expected a content security policy on the shell")
	}
}

func TestStateRequiresTheAdminToken(t *testing.T) {
	handler, life, _, key := setup(t)
	if response := call(t, handler, http.MethodGet, "/api/state", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous state=%d", response.Code)
	}
	// A diagnosis callback credential must not unlock the ledger read model.
	if response := call(t, handler, http.MethodGet, "/api/state", life.CallbackToken(key), ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("callback state=%d", response.Code)
	}
}

func TestLifecycleStatusesAreDerivedFromDurableFacts(t *testing.T) {
	handler, life, durable, key := setup(t)

	state := fetchState(t, handler)
	if len(state.Rows) != 1 || state.Rows[0].Status != StatusInvestigating || state.Rows[0].Diagnosis != nil {
		t.Fatalf("rows=%#v", state.Rows)
	}
	if !state.Demo || state.Rows[0].DetailsURL == "" {
		t.Fatalf("state=%#v", state)
	}

	// A callback credential cannot authorize a human approval.
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=approve&trigger="+string(key), life.CallbackToken(key), `{"actor":"me"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("callback approval=%d", response.Code)
	}

	diagnosis := `{"category":"code","confidence":0.5,"summary":"broken","evidence":[{"file":"x.go","line":4,"check":"TestX"}],"recommendedAction":"fix_code"}`
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=diagnosis&trigger="+string(key), life.CallbackToken(key), diagnosis); response.Code != http.StatusOK {
		t.Fatalf("diagnosis=%d", response.Code)
	}
	state = fetchState(t, handler)
	row := state.Rows[0]
	if row.Status != StatusAwaitingReview || row.Diagnosis == nil || row.Diagnosis.Summary != "broken" {
		t.Fatalf("row=%#v", row)
	}
	if !row.CanApprove || !row.CanFix || row.CanRetry {
		t.Fatalf("actions=%#v", row)
	}

	if response := call(t, handler, http.MethodPost, "/api/triggers?action=approve&trigger="+string(key), "admin", `{"actor":"me"}`); response.Code != http.StatusNoContent {
		t.Fatalf("approve=%d", response.Code)
	}
	if state = fetchState(t, handler); state.Rows[0].Status != StatusApproved {
		t.Fatalf("status=%s", state.Rows[0].Status)
	}

	if response := call(t, handler, http.MethodPost, "/api/triggers?action=fix&trigger="+string(key), "admin", ""); response.Code != http.StatusAccepted {
		t.Fatalf("fix=%d", response.Code)
	}
	state = fetchState(t, handler)
	if state.Rows[0].Status != StatusVerifying || state.Stats.Dispatched != 1 || state.Stats.AwaitingVerification != 1 {
		t.Fatalf("state=%#v", state)
	}

	// The verification outcome, not the dispatch, decides the final label.
	if err := durable.ResolveVerification(context.Background(), key, "0123456789abcdef", ledger.VerificationGreen, "CI passed", time.Now()); err != nil {
		t.Fatal(err)
	}
	if state = fetchState(t, handler); state.Rows[0].Status != StatusFixed || state.Stats.VerifiedGreen != 1 {
		t.Fatalf("state=%#v", state)
	}
}

func TestRetryOnlyFollowsAFailedDispatch(t *testing.T) {
	handler, life, _, key := setup(t)
	diagnosis := `{"category":"code","confidence":0.9,"summary":"broken","evidence":[{"file":"x.go"}],"recommendedAction":"fix_code"}`
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=diagnosis&trigger="+string(key), life.CallbackToken(key), diagnosis); response.Code != http.StatusOK {
		t.Fatalf("diagnosis=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=approve&trigger="+string(key), "admin", `{"actor":"me"}`); response.Code != http.StatusNoContent {
		t.Fatalf("approve=%d", response.Code)
	}
	// No dispatch has failed, so a retry must be refused.
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=retry&trigger="+string(key), "admin", `{"actor":"me"}`); response.Code != http.StatusConflict {
		t.Fatalf("premature retry=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=retry&trigger="+string(key), "", `{"actor":"me"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous retry=%d", response.Code)
	}
}

func TestFailedDispatchBecomesRetryable(t *testing.T) {
	handler, life, _, key := setupWith(t, fake{sendErr: context.DeadlineExceeded})
	diagnosis := `{"category":"code","confidence":0.9,"summary":"broken","evidence":[{"file":"x.go"}],"recommendedAction":"fix_code"}`
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=diagnosis&trigger="+string(key), life.CallbackToken(key), diagnosis); response.Code != http.StatusOK {
		t.Fatalf("diagnosis=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=approve&trigger="+string(key), "admin", `{"actor":"me"}`); response.Code != http.StatusNoContent {
		t.Fatalf("approve=%d", response.Code)
	}
	// The send fails inside AO, which the API surfaces as a conflict.
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=fix&trigger="+string(key), "admin", ""); response.Code != http.StatusConflict {
		t.Fatalf("fix=%d", response.Code)
	}
	state := fetchState(t, handler)
	row := state.Rows[0]
	if row.Status != StatusDispatchFailed || !row.CanRetry {
		t.Fatalf("row=%#v", row)
	}
	if state.Stats.AwaitingVerification != 0 {
		t.Fatal("a failed dispatch must not open a verification")
	}
}

func TestAutomationKillSwitchRequiresAnActor(t *testing.T) {
	handler, _, _, _ := setup(t)
	if response := call(t, handler, http.MethodPost, "/api/automation", "admin", `{"disabled":true}`); response.Code != http.StatusBadRequest {
		t.Fatalf("missing actor=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/automation", "admin", `{"disabled":true,"actor":"me"}`); response.Code != http.StatusNoContent {
		t.Fatalf("automation=%d", response.Code)
	}
	if state := fetchState(t, handler); !state.AutomationDisabled {
		t.Fatal("expected the kill switch to be reported as enabled")
	}
}

func TestCallbackTokenIsScoped(t *testing.T) {
	_, life, _, key := setup(t)
	if !life.VerifyCallbackToken(key, life.CallbackToken(key)) || life.VerifyCallbackToken(key+"x", life.CallbackToken(key)) {
		t.Fatal("callback scope failed")
	}
}

func TestSkippedEvaluationOffersNoActions(t *testing.T) {
	row := newRow(ledger.DashboardRow{Repository: "o/r", Evaluation: policy.OutcomeNonFailure}, false, time.Now())
	if row.Status != StatusSkipped || row.CanApprove || row.CanFix || row.CanRetry {
		t.Fatalf("row=%#v", row)
	}
}

func TestStalledInvestigationIsDerivedFromAge(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	source := ledger.DashboardRow{TriggerKey: "k", Repository: "o/r", Evaluation: policy.OutcomeReserved, SpawnOutcome: "spawned", SpawnCompletedAt: at}
	if row := newRow(source, false, at.Add(time.Minute)); row.Status != StatusInvestigating {
		t.Fatalf("status=%s", row.Status)
	}
	if row := newRow(source, false, at.Add(StaleInvestigation)); row.Status != StatusStalled {
		t.Fatalf("status=%s", row.Status)
	}
}

// A send the kill switch blocked never reached AO, and StartSendAttempt
// deliberately ignores those rows. The card must stay actionable, otherwise
// re-enabling automation leaves the trigger permanently stuck with no buttons.
func TestKillSwitchBlockedSendKeepsTheRowActionable(t *testing.T) {
	source := ledger.DashboardRow{
		TriggerKey: "k", Repository: "o/r", Evaluation: policy.OutcomeReserved, SpawnOutcome: "spawned",
		Diagnosis:   `{"category":"code","confidence":0.9,"summary":"broken","evidence":[{"file":"x.go"}],"recommendedAction":"fix_code"}`,
		SendOutcome: "blocked_kill_switch",
	}
	row := newRow(source, false, time.Now())
	if row.Status != StatusAutomationHeld {
		t.Fatalf("status=%s", row.Status)
	}
	if !row.CanApprove || !row.CanFix {
		t.Fatalf("a blocked send must leave the row actionable: %#v", row)
	}
	if row.CanRetry {
		t.Fatal("a blocked send is not a failed dispatch and must not offer retry")
	}
	// A send that genuinely reached AO still closes the row to further dispatch.
	source.SendOutcome = "sent"
	if sent := newRow(source, false, time.Now()); sent.CanApprove || sent.CanFix {
		t.Fatalf("a dispatched fix must not stay actionable: %#v", sent)
	}
}

// The retry authorization is durable and single-use. Spending it on a dispatch
// that cannot happen would leave the trigger with nothing sent and no
// authorization left.
func TestRetryDoesNotSpendTheAuthorizationWhenDispatchIsImpossible(t *testing.T) {
	handler, life, durable, key := setupWith(t, fake{sendErr: context.DeadlineExceeded})
	diagnosis := `{"category":"code","confidence":0.9,"summary":"broken","evidence":[{"file":"x.go"}],"recommendedAction":"fix_code"}`
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=diagnosis&trigger="+string(key), life.CallbackToken(key), diagnosis); response.Code != http.StatusOK {
		t.Fatalf("diagnosis=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=approve&trigger="+string(key), "admin", `{"actor":"me"}`); response.Code != http.StatusNoContent {
		t.Fatalf("approve=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=fix&trigger="+string(key), "admin", ""); response.Code != http.StatusConflict {
		t.Fatalf("fix=%d", response.Code)
	}
	// Kill switch on: the retry must be refused before anything is committed.
	if response := call(t, handler, http.MethodPost, "/api/automation", "admin", `{"disabled":true,"actor":"me"}`); response.Code != http.StatusNoContent {
		t.Fatalf("automation=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=retry&trigger="+string(key), "admin", `{"actor":"me"}`); response.Code != http.StatusConflict {
		t.Fatalf("retry under kill switch=%d", response.Code)
	}
	if authorizations, err := durable.Count(context.Background(), "send_attempts"); err != nil || authorizations != 1 {
		t.Fatalf("send attempts=%d err=%v", authorizations, err)
	}
	// With automation re-enabled the retry is still available: nothing was spent.
	if response := call(t, handler, http.MethodPost, "/api/automation", "admin", `{"disabled":false,"actor":"me"}`); response.Code != http.StatusNoContent {
		t.Fatalf("automation=%d", response.Code)
	}
	if response := call(t, handler, http.MethodPost, "/api/triggers?action=retry&trigger="+string(key), "admin", `{"actor":"me"}`); response.Code != http.StatusConflict {
		// The fake AO still fails the send, so the dispatch reports a conflict,
		// but the authorization must have been spent on a real attempt this time.
		t.Logf("retry dispatch reported %d", response.Code)
	}
	if attempts, err := durable.Count(context.Background(), "send_attempts"); err != nil || attempts != 2 {
		t.Fatalf("expected the retry to produce a second attempt: attempts=%d err=%v", attempts, err)
	}
}

func TestUnparseableStoredDiagnosisIsNotPresentedAsValidated(t *testing.T) {
	source := ledger.DashboardRow{TriggerKey: "k", Repository: "o/r", Evaluation: policy.OutcomeReserved, SpawnOutcome: "spawned", Diagnosis: `{"category":"nonsense"}`}
	row := newRow(source, false, time.Now())
	if row.Diagnosis != nil || row.CanApprove || row.CanFix {
		t.Fatalf("row=%#v", row)
	}
}
