package policy

import (
	"github.com/shitcodebykaushik/ao-watchtower/internal/config"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"testing"
)

func facts(conclusion string) domain.CheckSuiteFacts {
	r, _ := domain.ParseRepository("octo/repo")
	return domain.CheckSuiteFacts{ProviderID: 1, Repository: r, PullNumber: 7, HeadSHA: "abcdef0123456789", Conclusion: conclusion}
}
func TestEvaluateCheckSuite(t *testing.T) {
	mapped := config.Defaults()
	r, _ := domain.ParseRepository("octo/repo")
	mapped.RepositoryProjects = []domain.RepositoryProject{{Repository: r, AOProjectID: "project"}}
	cases := []struct {
		conclusion string
		c          config.Config
		outcome    string
	}{{"success", mapped, OutcomeNonFailure}, {"failure", config.Defaults(), OutcomeUnmappedRepository}, {"failure", mapped, OutcomeReserved}}
	for _, tc := range cases {
		got, err := EvaluateCheckSuite(facts(tc.conclusion), tc.c)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != tc.outcome {
			t.Fatalf("outcome=%s want %s", got.Outcome, tc.outcome)
		}
	}
}
