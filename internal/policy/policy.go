// Package policy contains the intentionally narrow CI-failure investigator rule.
package policy

import (
	"github.com/agent-orchestrator/ao-watchtower/internal/config"
	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
)

const (
	OutcomeReserved           = "reserved"
	OutcomeNonFailure         = "non_failure"
	OutcomeUnmappedRepository = "unmapped_repository"
)

type Evaluation struct {
	RuleID      string
	Outcome     string
	TriggerKey  domain.TriggerKey
	AOProjectID string
}

func EvaluateCheckSuite(facts domain.CheckSuiteFacts, configuration config.Config) (Evaluation, error) {
	if err := facts.Validate(); err != nil {
		return Evaluation{}, err
	}
	if facts.Conclusion != "failure" {
		return Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: OutcomeNonFailure}, nil
	}
	projectID, mapped := configuration.ProjectFor(facts.Repository)
	if !mapped {
		return Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: OutcomeUnmappedRepository}, nil
	}
	key, err := domain.NewCIFailureTriggerKey(facts.Repository, facts.PullNumber, facts.HeadSHA)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{RuleID: domain.InvestigateCIFailureRule, Outcome: OutcomeReserved, TriggerKey: key, AOProjectID: projectID}, nil
}
