package notify

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/prcomment"
	"github.com/shitcodebykaushik/ao-watchtower/internal/verification"
)

type call struct {
	repository domain.Repository
	pullNumber int64
	body       prcomment.Body
}

type fakePublisher struct {
	calls    []call
	failWith error
}

func (f *fakePublisher) Upsert(_ context.Context, repository domain.Repository, pullNumber int64, body prcomment.Body) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.calls = append(f.calls, call{repository: repository, pullNumber: pullNumber, body: body})
	return nil
}

type fakeStore struct {
	dashboard ledger.Dashboard
	failWith  error
}

func (f *fakeStore) Dashboard(context.Context) (ledger.Dashboard, error) {
	if f.failWith != nil {
		return ledger.Dashboard{}, f.failWith
	}
	return f.dashboard, nil
}

const validDiagnosis = `{"category":"code","confidence":0.93,"summary":"Add subtracts","evidence":[{"file":"calculator.go","line":4,"check":"TestAdd"}],"recommendedAction":"fix_code"}`

func newController(t *testing.T, store Store, publisher Publisher) *Controller {
	t.Helper()
	controller, err := New(store, publisher, "auto-fix", Options{Interval: time.Second, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func diagnosedRow() ledger.DashboardRow {
	return ledger.DashboardRow{TriggerKey: "github:acme/app:pull:7:head:abcdef0123456789:rule:investigate-ci-failure", Repository: "acme/app", PullNumber: 7, HeadSHA: "abcdef0123456789", Diagnosis: validDiagnosis}
}

func TestRunOncePublishesEachDiagnosisOnce(t *testing.T) {
	store := &fakeStore{dashboard: ledger.Dashboard{Rows: []ledger.DashboardRow{diagnosedRow()}}}
	publisher := &fakePublisher{}
	controller := newController(t, store, publisher)
	for attempt := 0; attempt < 3; attempt++ {
		if err := controller.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(publisher.calls) != 1 {
		t.Fatalf("calls=%d", len(publisher.calls))
	}
	published := publisher.calls[0]
	if published.repository.String() != "acme/app" || published.pullNumber != 7 {
		t.Fatalf("published=%#v", published)
	}
	if published.body.Marker == "" || published.body.Text == "" {
		t.Fatalf("body=%#v", published.body)
	}
}

func TestRunOnceSkipsRowsThatCannotBePublished(t *testing.T) {
	invalid := diagnosedRow()
	invalid.Diagnosis = `{"category":"nonsense","confidence":2}`
	unparseable := diagnosedRow()
	unparseable.Diagnosis = `not json`
	badRepository := diagnosedRow()
	badRepository.Repository = "not a repository"
	store := &fakeStore{dashboard: ledger.Dashboard{Rows: []ledger.DashboardRow{
		invalid,
		unparseable,
		badRepository,
		{TriggerKey: "", Repository: "acme/app", PullNumber: 7, Diagnosis: validDiagnosis},
		{TriggerKey: "k", Repository: "acme/app", PullNumber: 7},
	}}}
	publisher := &fakePublisher{}
	controller := newController(t, store, publisher)
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 0 {
		t.Fatalf("calls=%#v", publisher.calls)
	}
}

func TestVerificationSettledPublishesOnceWithADistinctMarker(t *testing.T) {
	store := &fakeStore{dashboard: ledger.Dashboard{Rows: []ledger.DashboardRow{diagnosedRow()}}}
	publisher := &fakePublisher{}
	controller := newController(t, store, publisher)
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	repository, err := domain.ParseRepository("acme/app")
	if err != nil {
		t.Fatal(err)
	}
	result := verification.Result{
		TriggerKey: domain.TriggerKey(diagnosedRow().TriggerKey), Repository: repository, PullNumber: 7,
		ObservedHeadSHA: "0123456789abcdef", Outcome: ledger.VerificationGreen, Detail: "CI passed",
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := controller.VerificationSettled(context.Background(), result); err != nil {
			t.Fatal(err)
		}
	}
	if len(publisher.calls) != 2 {
		t.Fatalf("calls=%d", len(publisher.calls))
	}
	// The outcome must not overwrite the diagnosis comment.
	if publisher.calls[0].body.Marker == publisher.calls[1].body.Marker {
		t.Fatalf("markers collided: %s", publisher.calls[0].body.Marker)
	}
}

func TestFailuresAreReported(t *testing.T) {
	store := &fakeStore{dashboard: ledger.Dashboard{Rows: []ledger.DashboardRow{diagnosedRow()}}}
	publisher := &fakePublisher{failWith: errors.New("gh unavailable")}
	controller := newController(t, store, publisher)
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected a publish failure to be reported")
	}
	store.failWith = errors.New("ledger unavailable")
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected a store failure to be reported")
	}
}

func greenResult(t *testing.T) verification.Result {
	t.Helper()
	repository, err := domain.ParseRepository("acme/app")
	if err != nil {
		t.Fatal(err)
	}
	return verification.Result{
		TriggerKey: domain.TriggerKey(diagnosedRow().TriggerKey), Repository: repository, PullNumber: 7,
		ObservedHeadSHA: "0123456789abcdef", Outcome: ledger.VerificationGreen, Detail: "CI passed",
	}
}

// A transient gh failure must not lose the outcome comment. The verification is
// already settled durably and the recorder never revisits it, so the controller
// itself has to carry the retry.
func TestOutcomeIsRetriedAfterATransientFailure(t *testing.T) {
	store := &fakeStore{dashboard: ledger.Dashboard{}}
	publisher := &fakePublisher{failWith: errors.New("gh unavailable")}
	controller := newController(t, store, publisher)
	result := greenResult(t)
	if err := controller.VerificationSettled(context.Background(), result); err == nil {
		t.Fatal("expected the publish failure to be reported")
	}
	if len(publisher.calls) != 0 {
		t.Fatalf("calls=%#v", publisher.calls)
	}
	// Still failing: the result must stay queued rather than being dropped.
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the retry to report the continuing failure")
	}
	publisher.failWith = nil
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 1 {
		t.Fatalf("expected exactly one published outcome, got %d", len(publisher.calls))
	}
	// Once published it must not be published again.
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 1 {
		t.Fatalf("calls=%d", len(publisher.calls))
	}
}

func TestQueuedOutcomesDoNotAccumulateDuplicates(t *testing.T) {
	store := &fakeStore{dashboard: ledger.Dashboard{}}
	publisher := &fakePublisher{failWith: errors.New("gh unavailable")}
	controller := newController(t, store, publisher)
	result := greenResult(t)
	for attempt := 0; attempt < 5; attempt++ {
		if err := controller.VerificationSettled(context.Background(), result); err == nil {
			t.Fatal("expected the publish failure to be reported")
		}
	}
	controller.mutex.Lock()
	queued := len(controller.pending)
	controller.mutex.Unlock()
	if queued != 1 {
		t.Fatalf("queued=%d want 1", queued)
	}
}

// The controller is reached from its own ticker and from the poller goroutine
// that settles verifications. Both touch the published set. Run under -race.
func TestConcurrentPublishingIsSafe(t *testing.T) {
	store := &fakeStore{dashboard: ledger.Dashboard{Rows: []ledger.DashboardRow{diagnosedRow()}}}
	publisher := &lockedPublisher{}
	controller := newController(t, store, publisher)
	result := greenResult(t)
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for attempt := 0; attempt < 25; attempt++ {
				_ = controller.RunOnce(context.Background())
				_ = controller.VerificationSettled(context.Background(), result)
			}
		}()
	}
	group.Wait()
}

// lockedPublisher is safe to call concurrently, so the test exercises the
// controller's synchronization rather than the fake's.
type lockedPublisher struct {
	mutex sync.Mutex
	calls int
}

func (p *lockedPublisher) Upsert(context.Context, domain.Repository, int64, prcomment.Body) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.calls++
	return nil
}

func TestNewValidatesInput(t *testing.T) {
	store, publisher := &fakeStore{}, &fakePublisher{}
	if _, err := New(nil, publisher, "review"); err == nil {
		t.Fatal("expected a missing store to be rejected")
	}
	if _, err := New(store, nil, "review"); err == nil {
		t.Fatal("expected a missing publisher to be rejected")
	}
	if _, err := New(store, publisher, ""); err == nil {
		t.Fatal("expected a missing mode to be rejected")
	}
	if _, err := New(store, publisher, "review", Options{Interval: -time.Second}); err == nil {
		t.Fatal("expected a negative interval to be rejected")
	}
	controller, err := New(store, publisher, "review", Options{Logger: log.New(io.Discard, "", 0)})
	if err != nil || controller.interval != DefaultInterval {
		t.Fatalf("controller=%#v err=%v", controller, err)
	}
}
