package domain

import "testing"

func TestParseRepositoryNormalizesAndRejectsAmbiguousValues(t *testing.T) {
	repository, err := ParseRepository("Octo-Org/Watch.Tower")
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}
	if got, want := repository.String(), "octo-org/watch.tower"; got != want {
		t.Fatalf("repository = %q, want %q", got, want)
	}

	for _, input := range []string{"octo", "octo/repo/extra", "octo /repo", "octo/:repo"} {
		if _, err := ParseRepository(input); err == nil {
			t.Errorf("ParseRepository(%q) succeeded", input)
		}
	}
}

func TestNewCIFailureTriggerKey(t *testing.T) {
	repository, err := ParseRepository("Octo/Watchtower")
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewCIFailureTriggerKey(repository, 42, "ABCDEF0123456789")
	if err != nil {
		t.Fatalf("NewCIFailureTriggerKey() error = %v", err)
	}
	const want = "github:octo/watchtower:pull:42:head:abcdef0123456789:rule:investigate-ci-failure"
	if string(key) != want {
		t.Fatalf("key = %q, want %q", key, want)
	}

	for _, test := range []struct {
		pull int64
		sha  string
	}{{0, "abcdef0"}, {42, "not-a-sha"}, {42, "abcdef"}} {
		if _, err := NewCIFailureTriggerKey(repository, test.pull, test.sha); err == nil {
			t.Errorf("NewCIFailureTriggerKey(%d, %q) succeeded", test.pull, test.sha)
		}
	}
}

func TestRepositoryProjectValidateRequiresNormalizedRepository(t *testing.T) {
	mapping := RepositoryProject{Repository: Repository{Owner: "Octo", Name: "repo"}, AOProjectID: "project"}
	if err := mapping.Validate(); err == nil {
		t.Fatal("Validate() succeeded for unnormalized repository")
	}
}

func TestDiagnosisValidate(t *testing.T) {
	valid := Diagnosis{Category: "code", Confidence: 0.93, Summary: "parser failure", RecommendedAction: "fix_code"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid diagnosis: %v", err)
	}
	for _, diagnosis := range []Diagnosis{
		{Category: "made-up", Confidence: 0.5, Summary: "x", RecommendedAction: "x"},
		{Category: "code", Confidence: 1.1, Summary: "x", RecommendedAction: "x"},
		{Category: "code", Confidence: 0.5, Summary: " ", RecommendedAction: "x"},
		{Category: "code", Confidence: 0.5, Summary: "x", RecommendedAction: " "},
	} {
		if err := diagnosis.Validate(); err == nil {
			t.Errorf("Diagnosis.Validate() succeeded for %#v", diagnosis)
		}
	}
}
