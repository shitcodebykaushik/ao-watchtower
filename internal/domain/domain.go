// Package domain contains durable Watchtower facts and their invariants.
package domain

import (
	"fmt"
	"strings"
	"time"
)

const InvestigateCIFailureRule = "investigate-ci-failure"

// Repository identifies a GitHub repository independently of a local checkout.
type Repository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

func ParseRepository(value string) (Repository, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validRepoPart(parts[0]) || !validRepoPart(parts[1]) {
		return Repository{}, fmt.Errorf("repository must be an owner/name pair")
	}
	return Repository{Owner: strings.ToLower(parts[0]), Name: strings.ToLower(parts[1])}, nil
}

func (r Repository) String() string { return r.Owner + "/" + r.Name }

func (r Repository) Validate() error {
	parsed, err := ParseRepository(r.String())
	if err != nil || parsed != r {
		if err != nil {
			return err
		}
		return fmt.Errorf("repository must be normalized")
	}
	return nil
}

func validRepoPart(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

// RepositoryProject maps a trusted GitHub repository to an existing AO project.
type RepositoryProject struct {
	Repository  Repository `json:"repository"`
	AOProjectID string     `json:"aoProjectId"`
}

func (m RepositoryProject) Validate() error {
	if err := m.Repository.Validate(); err != nil {
		return fmt.Errorf("repository: %w", err)
	}
	if strings.TrimSpace(m.AOProjectID) == "" || strings.TrimSpace(m.AOProjectID) != m.AOProjectID {
		return fmt.Errorf("AO project id is required")
	}
	return nil
}

// WebhookDelivery is the auditable result of receiving one GitHub delivery.
type WebhookDelivery struct {
	ID            string    `json:"id"`
	PayloadDigest string    `json:"payloadDigest"`
	ReceivedAt    time.Time `json:"receivedAt"`
}

// CheckSuiteFacts are the normalized provider facts relevant to the one MVP rule.
type CheckSuiteFacts struct {
	ProviderID    int64      `json:"providerId"`
	Repository    Repository `json:"repository"`
	PullNumber    int64      `json:"pullNumber"`
	HeadSHA       string     `json:"headSHA"`
	Conclusion    string     `json:"conclusion"`
	DetailsURL    string     `json:"detailsURL,omitempty"`
	CheckSuiteURL string     `json:"checkSuiteURL,omitempty"`
}

// TriggerKey is the idempotency boundary for a CI-failure investigator.
type TriggerKey string

func NewCIFailureTriggerKey(repository Repository, pullNumber int64, headSHA string) (TriggerKey, error) {
	if err := repository.Validate(); err != nil {
		return "", err
	}
	if pullNumber <= 0 {
		return "", fmt.Errorf("pull number must be positive")
	}
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	if !validSHA(headSHA) {
		return "", fmt.Errorf("head SHA must be 7 to 64 hexadecimal characters")
	}
	return TriggerKey(fmt.Sprintf("github:%s:pull:%d:head:%s:rule:%s", repository, pullNumber, headSHA, InvestigateCIFailureRule)), nil
}

func validSHA(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// TriggerRecord records an idempotent rule evaluation; display state is derived from it.
type TriggerRecord struct {
	Key       TriggerKey `json:"key"`
	RuleID    string     `json:"ruleId"`
	Outcome   string     `json:"outcome"`
	CreatedAt time.Time  `json:"createdAt"`
}

// SpawnAttempt records one AO mutation attempt and its observed command outcome.
type SpawnAttempt struct {
	TriggerKey  TriggerKey `json:"triggerKey"`
	AOProjectID string     `json:"aoProjectId"`
	AOSessionID string     `json:"aoSessionId,omitempty"`
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt time.Time  `json:"completedAt,omitempty"`
	Outcome     string     `json:"outcome"`
}

// HumanApproval is a durable, attributable permission for a later fix request.
type HumanApproval struct {
	TriggerKey TriggerKey `json:"triggerKey"`
	Actor      string     `json:"actor"`
	ApprovedAt time.Time  `json:"approvedAt"`
}

// DiagnosisEvidence identifies a concrete observation made by an investigator.
type DiagnosisEvidence struct {
	File  string `json:"file,omitempty"`
	Line  int64  `json:"line,omitempty"`
	Check string `json:"check,omitempty"`
}

// Diagnosis is the constrained payload returned by an investigator.
type Diagnosis struct {
	Category          string              `json:"category"`
	Confidence        float64             `json:"confidence"`
	Summary           string              `json:"summary"`
	Evidence          []DiagnosisEvidence `json:"evidence"`
	RecommendedAction string              `json:"recommendedAction"`
}

func (d Diagnosis) Validate() error {
	switch d.Category {
	case "code", "test", "infrastructure", "flaky", "configuration", "dependency", "unknown":
	default:
		return fmt.Errorf("unsupported diagnosis category")
	}
	if d.Confidence < 0 || d.Confidence > 1 {
		return fmt.Errorf("diagnosis confidence must be between 0 and 1")
	}
	if strings.TrimSpace(d.Summary) == "" {
		return fmt.Errorf("diagnosis summary is required")
	}
	if len(d.Summary) > 4096 {
		return fmt.Errorf("diagnosis summary is too long")
	}
	if strings.TrimSpace(d.RecommendedAction) == "" {
		return fmt.Errorf("diagnosis recommended action is required")
	}
	if len(d.RecommendedAction) > 128 {
		return fmt.Errorf("diagnosis recommended action is too long")
	}
	if len(d.Evidence) > 32 {
		return fmt.Errorf("diagnosis has too much evidence")
	}
	for index, evidence := range d.Evidence {
		if evidence.Line < 0 || len(evidence.File) > 1024 || len(evidence.Check) > 1024 {
			return fmt.Errorf("diagnosis evidence %d is invalid", index)
		}
		if strings.TrimSpace(evidence.File) == "" && strings.TrimSpace(evidence.Check) == "" {
			return fmt.Errorf("diagnosis evidence %d has no observation", index)
		}
	}
	return nil
}

// DiagnosisRecord retains both validation status and bounded raw payload for audit.
type DiagnosisRecord struct {
	TriggerKey TriggerKey `json:"triggerKey"`
	Raw        []byte     `json:"raw"`
	Diagnosis  *Diagnosis `json:"diagnosis,omitempty"`
	Valid      bool       `json:"valid"`
	RecordedAt time.Time  `json:"recordedAt"`
}

// SendAttempt records the outcome of a human-approved message sent to AO.
type SendAttempt struct {
	TriggerKey  TriggerKey `json:"triggerKey"`
	AOSessionID string     `json:"aoSessionId"`
	SentAt      time.Time  `json:"sentAt"`
	Outcome     string     `json:"outcome"`
}

// Validate ensures normalized check-suite facts are safe to persist and evaluate.
func (f CheckSuiteFacts) Validate() error {
	if f.ProviderID <= 0 {
		return fmt.Errorf("check suite id must be positive")
	}
	if err := f.Repository.Validate(); err != nil {
		return fmt.Errorf("repository: %w", err)
	}
	if f.PullNumber <= 0 {
		return fmt.Errorf("pull number must be positive")
	}
	if _, err := NewCIFailureTriggerKey(f.Repository, f.PullNumber, f.HeadSHA); err != nil {
		return fmt.Errorf("head SHA: %w", err)
	}
	if strings.TrimSpace(f.Conclusion) == "" || strings.TrimSpace(f.Conclusion) != f.Conclusion {
		return fmt.Errorf("check suite conclusion is required")
	}
	return nil
}
