package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

// DeferredSpawn is a reserved trigger that has no spawn attempt yet. That state
// has two causes, and both want the same treatment: the concurrency limit held
// the investigation back, or the process stopped between reserving the trigger
// and calling AO. Replaying it is safe because StartSpawnAttempt remains the
// single unique gate on reaching AO.
type DeferredSpawn struct {
	TriggerKey  domain.TriggerKey
	AOProjectID string
	Facts       domain.CheckSuiteFacts
	ReservedAt  time.Time
}

// ActiveInvestigations counts AO sessions Watchtower is still waiting on: a
// spawn succeeded but no diagnosis has been recorded for that trigger yet.
func (l *Ledger) ActiveInvestigations(ctx context.Context) (int, error) {
	var active int
	err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM spawn_attempts s WHERE s.outcome='spawned' AND NOT EXISTS (SELECT 1 FROM diagnoses d WHERE d.trigger_key=s.trigger_key)`).Scan(&active)
	return active, err
}

// DeferredSpawns lists reserved triggers that never reached a spawn attempt,
// oldest first, so the backlog drains in the order failures arrived.
func (l *Ledger) DeferredSpawns(ctx context.Context, limit int) ([]DeferredSpawn, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("deferred spawn limit must be positive")
	}
	rows, err := l.db.QueryContext(ctx, `SELECT r.trigger_key,r.ao_project_id,r.reserved_at,f.suite_id,f.owner,f.repo,f.pull_number,f.head_sha,f.conclusion,f.details_url,f.check_suite_url
FROM spawn_reservations r
JOIN evaluations e ON e.trigger_key=r.trigger_key
JOIN check_suite_facts f ON f.delivery_id=e.delivery_id
WHERE NOT EXISTS (SELECT 1 FROM spawn_attempts a WHERE a.trigger_key=r.trigger_key)
GROUP BY r.trigger_key
ORDER BY r.reserved_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deferred []DeferredSpawn
	for rows.Next() {
		var entry DeferredSpawn
		var reservedAt string
		if err := rows.Scan(&entry.TriggerKey, &entry.AOProjectID, &reservedAt, &entry.Facts.ProviderID,
			&entry.Facts.Repository.Owner, &entry.Facts.Repository.Name, &entry.Facts.PullNumber,
			&entry.Facts.HeadSHA, &entry.Facts.Conclusion, &entry.Facts.DetailsURL, &entry.Facts.CheckSuiteURL); err != nil {
			return nil, err
		}
		entry.ReservedAt, err = time.Parse(time.RFC3339Nano, reservedAt)
		if err != nil {
			return nil, fmt.Errorf("parse reservation time: %w", err)
		}
		if err := entry.Facts.Validate(); err != nil {
			return nil, fmt.Errorf("deferred spawn facts: %w", err)
		}
		deferred = append(deferred, entry)
	}
	return deferred, rows.Err()
}

// DispatchedFixesSince counts code-modifying sends that actually reached AO in
// a window. It backs the daily fix budget, which bounds how much unattended
// change automation may cause in a day.
func (l *Ledger) DispatchedFixesSince(ctx context.Context, since time.Time) (int, error) {
	if since.IsZero() {
		return 0, fmt.Errorf("window start is required")
	}
	var dispatched int
	err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM send_attempts WHERE outcome='sent' AND completed_at>=?`, timestamp(since)).Scan(&dispatched)
	return dispatched, err
}
