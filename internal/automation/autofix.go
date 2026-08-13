// Package automation runs explicitly enabled repository-scoped automations.
package automation

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
)

const DefaultMinimumConfidence = 0.8

type Lifecycle interface {
	ApproveFix(context.Context, domain.TriggerKey, string) error
	FixWithAO(context.Context, domain.TriggerKey) error
}

// Gate is an additional, repository-owned condition an automatic fix must pass.
// It is an interface so this package stays independent of how the policy is
// expressed; production wiring supplies the committed .watchtower.json policy.
type Gate interface {
	AllowAutoFix(domain.Diagnosis) (bool, string)
}

// Options carries the optional controller settings.
type Options struct {
	// DailyFixBudget bounds how many code-modifying dispatches automation may
	// cause in a rolling 24 hours. Zero means unbounded.
	DailyFixBudget int
	// Gate is consulted after the built-in eligibility rules pass.
	Gate Gate
}

type Controller struct {
	ledger       *ledger.Ledger
	lifecycle    Lifecycle
	actor        string
	minimum      float64
	interval     time.Duration
	logger       *log.Logger
	processing   map[domain.TriggerKey]bool
	budget       int
	gate         Gate
	now          func() time.Time
	refused      map[domain.TriggerKey]bool
	budgetPaused bool
}

func New(durableLedger *ledger.Ledger, lifecycle Lifecycle, actor string, minimum float64, interval time.Duration, logger *log.Logger, options ...Options) (*Controller, error) {
	if durableLedger == nil || lifecycle == nil {
		return nil, fmt.Errorf("ledger and lifecycle are required")
	}
	if strings.TrimSpace(actor) == "" || minimum < 0 || minimum > 1 || interval <= 0 {
		return nil, fmt.Errorf("valid actor, confidence, and interval are required")
	}
	if logger == nil {
		logger = log.Default()
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("only one automation option is supported")
	}
	var option Options
	if len(options) == 1 {
		option = options[0]
	}
	if option.DailyFixBudget < 0 {
		return nil, fmt.Errorf("daily fix budget must not be negative")
	}
	return &Controller{
		ledger: durableLedger, lifecycle: lifecycle, actor: actor, minimum: minimum,
		interval: interval, logger: logger, processing: make(map[domain.TriggerKey]bool),
		budget: option.DailyFixBudget, gate: option.Gate, now: time.Now,
		refused: make(map[domain.TriggerKey]bool),
	}, nil
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

func (c *Controller) RunOnce(ctx context.Context) error {
	dashboard, err := c.ledger.Dashboard(ctx)
	if err != nil {
		return err
	}
	if dashboard.AutomationDisabled {
		return nil
	}
	for _, row := range dashboard.Rows {
		key := domain.TriggerKey(row.TriggerKey)
		if key == "" || row.SpawnOutcome != "spawned" || row.Diagnosis == "" || row.SendOutcome != "" || c.processing[key] {
			continue
		}
		diagnosis, valid, err := c.ledger.LatestValidDiagnosis(ctx, key)
		if err != nil {
			return err
		}
		if !eligible(diagnosis, valid, c.minimum) {
			continue
		}
		if c.gate != nil {
			allowed, reason := c.gate.AllowAutoFix(diagnosis)
			if !allowed {
				// Log the refusal once. The diagnosis stays visible in the
				// dashboard for a human to approve deliberately.
				if !c.refused[key] {
					c.refused[key] = true
					c.logger.Printf("Auto-fix refused by repository policy %s: %s", key, reason)
				}
				continue
			}
		}
		// The budget is checked immediately before each dispatch rather than
		// once per cycle, so a burst of eligible diagnoses cannot overshoot it.
		within, err := c.withinBudget(ctx)
		if err != nil {
			return err
		}
		if !within {
			// Log once per pause. A spent budget with an eligible diagnosis
			// waiting is exactly the steady state the budget creates, and this
			// loop runs every second.
			if !c.budgetPaused {
				c.budgetPaused = true
				c.logger.Printf("Auto-fix paused: daily budget of %d dispatched fixes is spent", c.budget)
			}
			return nil
		}
		if c.budgetPaused {
			c.budgetPaused = false
			c.logger.Printf("Auto-fix resumed: daily budget of %d dispatched fixes has capacity again", c.budget)
		}
		c.processing[key] = true
		if row.Approval == "" {
			if err := c.lifecycle.ApproveFix(ctx, key, c.actor); err != nil {
				delete(c.processing, key)
				return fmt.Errorf("auto-approve %s: %w", key, err)
			}
			c.logger.Printf("Auto-fix approved %s at %.0f%% confidence", key, diagnosis.Confidence*100)
		}
		if err := c.lifecycle.FixWithAO(ctx, key); err != nil {
			delete(c.processing, key)
			return fmt.Errorf("dispatch auto-fix %s: %w", key, err)
		}
		c.logger.Printf("Auto-fix dispatched %s", key)
	}
	return nil
}

func (c *Controller) runOnce(ctx context.Context) {
	if err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
		c.logger.Printf("Auto-fix: %v", err)
	}
}

// withinBudget reports whether another automatic fix may be dispatched inside
// the rolling 24-hour window.
func (c *Controller) withinBudget(ctx context.Context) (bool, error) {
	if c.budget <= 0 {
		return true, nil
	}
	dispatched, err := c.ledger.DispatchedFixesSince(ctx, c.now().Add(-24*time.Hour))
	if err != nil {
		return false, err
	}
	return dispatched < c.budget, nil
}

func eligible(diagnosis domain.Diagnosis, valid bool, minimum float64) bool {
	return valid && diagnosis.Category == "code" && diagnosis.RecommendedAction == "fix_code" && diagnosis.Confidence >= minimum && len(diagnosis.Evidence) > 0
}
