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

type Controller struct {
	ledger     *ledger.Ledger
	lifecycle  Lifecycle
	actor      string
	minimum    float64
	interval   time.Duration
	logger     *log.Logger
	processing map[domain.TriggerKey]bool
}

func New(durableLedger *ledger.Ledger, lifecycle Lifecycle, actor string, minimum float64, interval time.Duration, logger *log.Logger) (*Controller, error) {
	if durableLedger == nil || lifecycle == nil {
		return nil, fmt.Errorf("ledger and lifecycle are required")
	}
	if strings.TrimSpace(actor) == "" || minimum < 0 || minimum > 1 || interval <= 0 {
		return nil, fmt.Errorf("valid actor, confidence, and interval are required")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Controller{ledger: durableLedger, lifecycle: lifecycle, actor: actor, minimum: minimum, interval: interval, logger: logger, processing: make(map[domain.TriggerKey]bool)}, nil
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

func eligible(diagnosis domain.Diagnosis, valid bool, minimum float64) bool {
	return valid && diagnosis.Category == "code" && diagnosis.RecommendedAction == "fix_code" && diagnosis.Confidence >= minimum && len(diagnosis.Evidence) > 0
}
