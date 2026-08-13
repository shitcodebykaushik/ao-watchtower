package verification

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/polling"
)

const (
	failedSHA = "abcdef0123456789"
	repairSHA = "0123456789abcdef"
)

type resolution struct {
	key       domain.TriggerKey
	headSHA   string
	outcome   string
	detail    string
	resolveAt time.Time
}

type fakeStore struct {
	mutex       sync.Mutex
	pending     []ledger.PendingVerification
	resolutions []resolution
	failWith    error
}

func (f *fakeStore) PendingVerifications(context.Context) ([]ledger.PendingVerification, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	return append([]ledger.PendingVerification(nil), f.pending...), nil
}

func (f *fakeStore) ResolveVerification(_ context.Context, key domain.TriggerKey, observedHeadSHA, outcome, detail string, at time.Time) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.resolutions = append(f.resolutions, resolution{key: key, headSHA: observedHeadSHA, outcome: outcome, detail: detail, resolveAt: at})
	// The real ledger only ever moves a verification out of the awaiting state,
	// so the fake mirrors that by dropping it from the pending set.
	remaining := f.pending[:0]
	for _, entry := range f.pending {
		if entry.TriggerKey != key {
			remaining = append(remaining, entry)
		}
	}
	f.pending = remaining
	return nil
}

type recordingNotifier struct {
	results []Result
	failure error
}

func (r *recordingNotifier) VerificationSettled(_ context.Context, result Result) error {
	r.results = append(r.results, result)
	return r.failure
}

func testRepository(t *testing.T) domain.Repository {
	t.Helper()
	repository, err := domain.ParseRepository("acme/app")
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func pendingEntry(t *testing.T, startedAt time.Time) ledger.PendingVerification {
	t.Helper()
	repository := testRepository(t)
	key, err := domain.NewCIFailureTriggerKey(repository, 7, failedSHA)
	if err != nil {
		t.Fatal(err)
	}
	return ledger.PendingVerification{TriggerKey: key, Repository: repository, PullNumber: 7, DispatchedHeadSHA: failedSHA, StartedAt: startedAt}
}

func newRecorder(t *testing.T, store Store, at time.Time, options Options) *Recorder {
	t.Helper()
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}
	recorder, err := NewRecorder(store, options)
	if err != nil {
		t.Fatal(err)
	}
	recorder.now = func() time.Time { return at }
	return recorder
}

func TestObserveRecordsTerminalOutcomes(t *testing.T) {
	startedAt := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name        string
		observation *polling.Observation
		elapsed     time.Duration
		outcome     string
		headSHA     string
	}{
		{
			name:        "repair commit passed CI",
			observation: &polling.Observation{PullNumber: 7, HeadSHA: repairSHA, Conclusion: "success"},
			outcome:     ledger.VerificationGreen,
			headSHA:     repairSHA,
		},
		{
			name:        "repair commit failed CI",
			observation: &polling.Observation{PullNumber: 7, HeadSHA: repairSHA, Conclusion: "failure"},
			outcome:     ledger.VerificationStillFailing,
			headSHA:     repairSHA,
		},
		{
			name:        "no repair commit before the timeout",
			observation: &polling.Observation{PullNumber: 7, HeadSHA: failedSHA, Conclusion: "failure"},
			elapsed:     2 * time.Hour,
			outcome:     ledger.VerificationAbandoned,
			headSHA:     failedSHA,
		},
		{
			name:    "pull request left the open set and the timeout expired",
			elapsed: 2 * time.Hour,
			outcome: ledger.VerificationAbandoned,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeStore{pending: []ledger.PendingVerification{pendingEntry(t, startedAt)}}
			notifier := &recordingNotifier{}
			recorder := newRecorder(t, store, startedAt.Add(testCase.elapsed), Options{Timeout: time.Hour, Notifier: notifier})
			var observations []polling.Observation
			if testCase.observation != nil {
				observations = append(observations, *testCase.observation)
			}
			if err := recorder.Observe(context.Background(), testRepository(t), observations); err != nil {
				t.Fatal(err)
			}
			if len(store.resolutions) != 1 {
				t.Fatalf("resolutions=%#v", store.resolutions)
			}
			settled := store.resolutions[0]
			if settled.outcome != testCase.outcome || settled.headSHA != testCase.headSHA {
				t.Fatalf("outcome=%s headSHA=%s", settled.outcome, settled.headSHA)
			}
			if settled.detail == "" {
				t.Fatal("expected a recorded detail")
			}
			if len(notifier.results) != 1 || notifier.results[0].Outcome != testCase.outcome {
				t.Fatalf("notifier results=%#v", notifier.results)
			}
		})
	}
}

func TestObserveKeepsWaitingBeforeTheTimeout(t *testing.T) {
	startedAt := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name         string
		observations []polling.Observation
	}{
		{name: "repair commit not pushed yet", observations: []polling.Observation{{PullNumber: 7, HeadSHA: failedSHA, Conclusion: "failure"}}},
		{name: "pull request not currently listed"},
		{name: "conclusion is neither success nor failure", observations: []polling.Observation{{PullNumber: 7, HeadSHA: repairSHA, Conclusion: "cancelled"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeStore{pending: []ledger.PendingVerification{pendingEntry(t, startedAt)}}
			recorder := newRecorder(t, store, startedAt.Add(time.Minute), Options{Timeout: time.Hour})
			if err := recorder.Observe(context.Background(), testRepository(t), testCase.observations); err != nil {
				t.Fatal(err)
			}
			if len(store.resolutions) != 0 {
				t.Fatalf("expected no resolution, got %#v", store.resolutions)
			}
		})
	}
}

func TestObserveIgnoresOtherRepositoriesAndSettlesOnce(t *testing.T) {
	startedAt := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{pending: []ledger.PendingVerification{pendingEntry(t, startedAt)}}
	recorder := newRecorder(t, store, startedAt.Add(time.Minute), Options{Timeout: time.Hour})
	other, err := domain.ParseRepository("other/app")
	if err != nil {
		t.Fatal(err)
	}
	observations := []polling.Observation{{PullNumber: 7, HeadSHA: repairSHA, Conclusion: "success"}}
	if err := recorder.Observe(context.Background(), other, observations); err != nil {
		t.Fatal(err)
	}
	if len(store.resolutions) != 0 {
		t.Fatalf("a different repository must not settle the verification: %#v", store.resolutions)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := recorder.Observe(context.Background(), testRepository(t), observations); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.resolutions) != 1 {
		t.Fatalf("repeated observation must settle exactly once: %#v", store.resolutions)
	}
}

func TestNotifierFailureDoesNotFailTheCycle(t *testing.T) {
	startedAt := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{pending: []ledger.PendingVerification{pendingEntry(t, startedAt)}}
	notifier := &recordingNotifier{failure: context.DeadlineExceeded}
	recorder := newRecorder(t, store, startedAt.Add(time.Minute), Options{Timeout: time.Hour, Notifier: notifier})
	err := recorder.Observe(context.Background(), testRepository(t), []polling.Observation{{PullNumber: 7, HeadSHA: repairSHA, Conclusion: "success"}})
	if err != nil {
		t.Fatalf("a notifier failure must not fail the poll cycle: %v", err)
	}
	if len(store.resolutions) != 1 {
		t.Fatalf("resolutions=%#v", store.resolutions)
	}
}

func TestNewRecorderValidatesInput(t *testing.T) {
	if _, err := NewRecorder(nil); err == nil {
		t.Fatal("expected a missing store to be rejected")
	}
	if _, err := NewRecorder(&fakeStore{}, Options{}, Options{}); err == nil {
		t.Fatal("expected more than one option set to be rejected")
	}
	if _, err := NewRecorder(&fakeStore{}, Options{Timeout: -time.Second}); err == nil {
		t.Fatal("expected a negative timeout to be rejected")
	}
	recorder, err := NewRecorder(&fakeStore{}, Options{Logger: log.New(io.Discard, "", 0)})
	if err != nil || recorder.timeout != DefaultTimeout {
		t.Fatalf("recorder=%#v err=%v", recorder, err)
	}
}
