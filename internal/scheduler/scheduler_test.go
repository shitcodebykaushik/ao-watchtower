package scheduler

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/service"
)

type fakeStore struct {
	deferred  []ledger.DeferredSpawn
	lastLimit int
	failWith  error
}

func (f *fakeStore) DeferredSpawns(_ context.Context, limit int) ([]ledger.DeferredSpawn, error) {
	f.lastLimit = limit
	if f.failWith != nil {
		return nil, f.failWith
	}
	return append([]ledger.DeferredSpawn(nil), f.deferred...), nil
}

type fakeStarter struct {
	capacity int
	resumed  []domain.TriggerKey
	failWith error
}

func (f *fakeStarter) HasCapacity(context.Context) (bool, error) { return f.capacity > 0, nil }

func (f *fakeStarter) ResumeDeferredSpawn(_ context.Context, entry ledger.DeferredSpawn) error {
	if f.failWith != nil {
		return f.failWith
	}
	if f.capacity <= 0 {
		return service.ErrAtCapacity
	}
	f.capacity--
	f.resumed = append(f.resumed, entry.TriggerKey)
	return nil
}

func deferredSpawn(t *testing.T, pullNumber int64, reservedAt time.Time) ledger.DeferredSpawn {
	t.Helper()
	repository, err := domain.ParseRepository("acme/app")
	if err != nil {
		t.Fatal(err)
	}
	key, err := domain.NewCIFailureTriggerKey(repository, pullNumber, "abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	return ledger.DeferredSpawn{
		TriggerKey:  key,
		AOProjectID: "app",
		Facts:       domain.CheckSuiteFacts{ProviderID: 1, Repository: repository, PullNumber: pullNumber, HeadSHA: "abcdef0123456789", Conclusion: "failure"},
		ReservedAt:  reservedAt,
	}
}

func newController(t *testing.T, store Store, starter Starter) *Controller {
	t.Helper()
	controller, err := New(store, starter, Options{Interval: time.Second, Batch: 10, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func TestRunOnceDrainsBacklogUpToCapacity(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{deferred: []ledger.DeferredSpawn{
		deferredSpawn(t, 1, at),
		deferredSpawn(t, 2, at.Add(time.Minute)),
		deferredSpawn(t, 3, at.Add(2*time.Minute)),
	}}
	starter := &fakeStarter{capacity: 2}
	controller := newController(t, store, starter)
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(starter.resumed) != 2 {
		t.Fatalf("resumed=%#v", starter.resumed)
	}
	// The backlog drains oldest failure first.
	if starter.resumed[0] != store.deferred[0].TriggerKey || starter.resumed[1] != store.deferred[1].TriggerKey {
		t.Fatalf("resumed out of order: %#v", starter.resumed)
	}
	if store.lastLimit != 10 {
		t.Fatalf("limit=%d", store.lastLimit)
	}
}

func TestRunOnceStopsQuietlyWhenCapacityRunsOutMidBatch(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{deferred: []ledger.DeferredSpawn{deferredSpawn(t, 1, at), deferredSpawn(t, 2, at.Add(time.Minute))}}
	// Capacity reports available but the resume itself defers, which is the race
	// between the check and the durable attempt.
	starter := &fakeStarter{capacity: 1, failWith: service.ErrAtCapacity}
	controller := newController(t, store, starter)
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatalf("a deferred resume must not be reported as a failure: %v", err)
	}
	if len(starter.resumed) != 0 {
		t.Fatalf("resumed=%#v", starter.resumed)
	}
}

func TestRunOnceReportsRealFailures(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{deferred: []ledger.DeferredSpawn{deferredSpawn(t, 1, at)}}
	starter := &fakeStarter{capacity: 1, failWith: errors.New("AO unreachable")}
	controller := newController(t, store, starter)
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected a resume failure to be reported")
	}
	store.failWith = errors.New("ledger unavailable")
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected a store failure to be reported")
	}
}

func TestRunOnceDoesNothingWithoutCapacity(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{deferred: []ledger.DeferredSpawn{deferredSpawn(t, 1, at)}}
	starter := &fakeStarter{capacity: 0}
	controller := newController(t, store, starter)
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(starter.resumed) != 0 {
		t.Fatalf("resumed=%#v", starter.resumed)
	}
}

func TestNewValidatesInput(t *testing.T) {
	store, starter := &fakeStore{}, &fakeStarter{}
	if _, err := New(nil, starter); err == nil {
		t.Fatal("expected a missing store to be rejected")
	}
	if _, err := New(store, nil); err == nil {
		t.Fatal("expected a missing starter to be rejected")
	}
	if _, err := New(store, starter, Options{}, Options{}); err == nil {
		t.Fatal("expected more than one option set to be rejected")
	}
	if _, err := New(store, starter, Options{Interval: -time.Second}); err == nil {
		t.Fatal("expected a negative interval to be rejected")
	}
	if _, err := New(store, starter, Options{Batch: -1}); err == nil {
		t.Fatal("expected a negative batch to be rejected")
	}
	controller, err := New(store, starter, Options{Logger: log.New(io.Discard, "", 0)})
	if err != nil || controller.interval != DefaultInterval || controller.batch != defaultBatch {
		t.Fatalf("controller=%#v err=%v", controller, err)
	}
}
