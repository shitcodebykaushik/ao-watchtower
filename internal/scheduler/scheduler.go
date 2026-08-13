// Package scheduler replays reserved triggers that never reached AO. A trigger
// lands here for two reasons: the concurrent investigation limit held it back,
// or Watchtower stopped between committing the reservation and calling AO.
// Without this sweep the first case would silently drop work and the second
// would lose a failure permanently, because a replayed webhook is deduplicated
// by delivery id and never re-reserves the trigger.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
)

// DefaultInterval is how often the backlog is checked.
const DefaultInterval = 5 * time.Second

// defaultBatch bounds how many deferred triggers one sweep will consider.
const defaultBatch = 50

// Store is the durable subset of the ledger the scheduler reads.
type Store interface {
	DeferredSpawns(context.Context, int) ([]ledger.DeferredSpawn, error)
}

// Starter is the lifecycle operation the scheduler drives. StartSpawnAttempt
// inside it remains the single unique gate on reaching AO, so replaying a
// trigger that another path already spawned is a no-op rather than a duplicate.
type Starter interface {
	HasCapacity(context.Context) (bool, error)
	ResumeDeferredSpawn(context.Context, ledger.DeferredSpawn) error
}

type Controller struct {
	store    Store
	starter  Starter
	interval time.Duration
	batch    int
	logger   *log.Logger
}

// Options carries the optional controller settings.
type Options struct {
	Interval time.Duration
	Batch    int
	Logger   *log.Logger
}

func New(store Store, starter Starter, options ...Options) (*Controller, error) {
	if store == nil || starter == nil {
		return nil, fmt.Errorf("scheduler store and starter are required")
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("only one scheduler option is supported")
	}
	var option Options
	if len(options) == 1 {
		option = options[0]
	}
	if option.Interval == 0 {
		option.Interval = DefaultInterval
	}
	if option.Interval < 0 {
		return nil, fmt.Errorf("scheduler interval must be positive")
	}
	if option.Batch == 0 {
		option.Batch = defaultBatch
	}
	if option.Batch < 0 {
		return nil, fmt.Errorf("scheduler batch must be positive")
	}
	if option.Logger == nil {
		option.Logger = log.Default()
	}
	return &Controller{store: store, starter: starter, interval: option.Interval, batch: option.Batch, logger: option.Logger}, nil
}

func (c *Controller) Run(ctx context.Context) {
	c.runOnce(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// RunOnce drains as much of the deferred backlog as current capacity allows,
// oldest failure first.
func (c *Controller) RunOnce(ctx context.Context) error {
	deferred, err := c.store.DeferredSpawns(ctx, c.batch)
	if err != nil {
		return err
	}
	for _, entry := range deferred {
		available, err := c.starter.HasCapacity(ctx)
		if err != nil {
			return err
		}
		if !available {
			return nil
		}
		if err := c.starter.ResumeDeferredSpawn(ctx, entry); err != nil {
			if errors.Is(err, domain.ErrInvestigationDeferred) {
				return nil
			}
			return fmt.Errorf("resume %s: %w", entry.TriggerKey, err)
		}
		c.logger.Printf("Started deferred investigation %s", entry.TriggerKey)
	}
	return nil
}

func (c *Controller) runOnce(ctx context.Context) {
	if err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
		c.logger.Printf("Scheduler: %v", err)
	}
}
