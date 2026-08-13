package web

import (
	"encoding/json"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
)

// StaleInvestigation is how long a spawned investigator may go without
// submitting a diagnosis before the dashboard flags it. It is a display
// judgement derived at read time, never a durable fact.
const StaleInvestigation = 20 * time.Minute

// Lifecycle status labels. These are derived from durable facts on every read,
// so the ledger never has to keep a second, drifting source of truth.
const (
	StatusSkipped        = "skipped"
	StatusQueued         = "queued"
	StatusInvestigating  = "investigating"
	StatusStalled        = "stalled"
	StatusOwnedElsewhere = "owned_elsewhere"
	StatusSpawnFailed    = "spawn_failed"
	StatusAwaitingReview = "awaiting_approval"
	StatusApproved       = "approved"
	StatusDispatchFailed = "dispatch_failed"
	StatusVerifying      = "verifying"
	StatusFixed          = "fixed"
	StatusFixDidNotWork  = "fix_did_not_work"
	StatusUnverified     = "unverified"
	StatusAutomationHeld = "held_by_kill_switch"
)

// Row is the JSON shape the dashboard renders. It flattens one ledger row and
// adds the derived status, the parsed diagnosis, and the actions currently
// available, so the browser needs no knowledge of the lifecycle rules.
type Row struct {
	TriggerKey   string            `json:"triggerKey"`
	Repository   string            `json:"repository"`
	PullNumber   int64             `json:"pullNumber"`
	HeadSHA      string            `json:"headSHA"`
	Conclusion   string            `json:"conclusion"`
	DetailsURL   string            `json:"detailsURL,omitempty"`
	Evaluation   string            `json:"evaluation"`
	Status       string            `json:"status"`
	ProjectID    string            `json:"projectId,omitempty"`
	SessionID    string            `json:"sessionId,omitempty"`
	SpawnOutcome string            `json:"spawnOutcome,omitempty"`
	Diagnosis    *domain.Diagnosis `json:"diagnosis,omitempty"`
	Approval     string            `json:"approval,omitempty"`
	SendOutcome  string            `json:"sendOutcome,omitempty"`
	SendDetail   string            `json:"sendDetail,omitempty"`
	Verification string            `json:"verification,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	CanApprove   bool              `json:"canApprove"`
	CanFix       bool              `json:"canFix"`
	CanRetry     bool              `json:"canRetry"`
}

// State is the whole dashboard payload.
type State struct {
	Demo               bool         `json:"demo"`
	AutomationDisabled bool         `json:"automationDisabled"`
	Stats              StatsPayload `json:"stats"`
	Rows               []Row        `json:"rows"`
}

// StatsPayload presents the aggregate ledger counts plus the two rates that
// tell an operator whether the automation is actually earning its keep.
type StatsPayload struct {
	Triggers             int     `json:"triggers"`
	Spawned              int     `json:"spawned"`
	ClaimConflicts       int     `json:"claimConflicts"`
	ValidDiagnoses       int     `json:"validDiagnoses"`
	Approvals            int     `json:"approvals"`
	Dispatched           int     `json:"dispatched"`
	AwaitingVerification int     `json:"awaitingVerification"`
	VerifiedGreen        int     `json:"verifiedGreen"`
	StillFailing         int     `json:"stillFailing"`
	Abandoned            int     `json:"abandoned"`
	RepairSuccessRate    float64 `json:"repairSuccessRate"`
	MedianTimeToGreen    string  `json:"medianTimeToGreen"`
}

func newStatsPayload(stats ledger.Stats) StatsPayload {
	median := ""
	if stats.MedianTimeToGreen > 0 {
		median = stats.MedianTimeToGreen.Round(time.Second).String()
	}
	return StatsPayload{
		Triggers: stats.Triggers, Spawned: stats.Spawned, ClaimConflicts: stats.ClaimConflicts,
		ValidDiagnoses: stats.ValidDiagnoses, Approvals: stats.Approvals, Dispatched: stats.Dispatched,
		AwaitingVerification: stats.AwaitingVerification, VerifiedGreen: stats.VerifiedGreen,
		StillFailing: stats.StillFailing, Abandoned: stats.Abandoned,
		RepairSuccessRate: stats.RepairSuccessRate(), MedianTimeToGreen: median,
	}
}

// newRow converts one durable ledger row into its display form. A stored
// diagnosis that no longer parses is dropped rather than shown, because the
// dashboard must never present unvalidated agent text as a validated finding.
func newRow(source ledger.DashboardRow, automationDisabled bool, at time.Time) Row {
	row := Row{
		TriggerKey: source.TriggerKey, Repository: source.Repository, PullNumber: source.PullNumber,
		HeadSHA: source.HeadSHA, Conclusion: source.Conclusion, DetailsURL: source.DetailsURL,
		Evaluation: source.Evaluation, ProjectID: source.ProjectID, SessionID: source.SessionID,
		SpawnOutcome: source.SpawnOutcome, Approval: source.Approval, SendOutcome: source.SendOutcome,
		SendDetail: source.SendDetail, Verification: source.Verification, CreatedAt: source.CreatedAt,
	}
	if source.Diagnosis != "" {
		var diagnosis domain.Diagnosis
		if json.Unmarshal([]byte(source.Diagnosis), &diagnosis) == nil && diagnosis.Validate() == nil {
			row.Diagnosis = &diagnosis
		}
	}
	row.Status = deriveStatus(source, row.Diagnosis != nil, automationDisabled, at)
	row.CanApprove = row.TriggerKey != "" && row.Diagnosis != nil && source.Approval == "" && source.SendOutcome == ""
	row.CanFix = row.TriggerKey != "" && row.Diagnosis != nil && source.SendOutcome == ""
	row.CanRetry = row.TriggerKey != "" && source.SendOutcome == "failed"
	return row
}

// deriveStatus reduces the durable facts of one trigger to a single label an
// operator can scan. Order matters: the latest fact that settles the outcome
// wins, so a verified repair is never displayed as merely dispatched.
func deriveStatus(source ledger.DashboardRow, hasDiagnosis, automationDisabled bool, at time.Time) string {
	if source.TriggerKey == "" || source.Evaluation != policy.OutcomeReserved {
		return StatusSkipped
	}
	switch source.Verification {
	case ledger.VerificationGreen:
		return StatusFixed
	case ledger.VerificationStillFailing:
		return StatusFixDidNotWork
	case ledger.VerificationAbandoned:
		return StatusUnverified
	case ledger.VerificationAwaiting:
		return StatusVerifying
	}
	switch source.SendOutcome {
	case "sent":
		return StatusVerifying
	case "failed":
		return StatusDispatchFailed
	case "blocked_kill_switch":
		return StatusAutomationHeld
	}
	switch source.SpawnOutcome {
	case "":
		if automationDisabled {
			return StatusAutomationHeld
		}
		return StatusQueued
	case "claim_conflict":
		return StatusOwnedElsewhere
	case "failed":
		return StatusSpawnFailed
	case "blocked_kill_switch":
		return StatusAutomationHeld
	}
	if !hasDiagnosis {
		if !source.SpawnCompletedAt.IsZero() && at.Sub(source.SpawnCompletedAt) >= StaleInvestigation {
			return StatusStalled
		}
		return StatusInvestigating
	}
	if source.Approval == "" {
		return StatusAwaitingReview
	}
	return StatusApproved
}
