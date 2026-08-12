package config

import (
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

func TestDefaultsAreValid(t *testing.T) {
	config := Defaults()
	if err := config.Validate(); err != nil {
		t.Fatalf("Defaults().Validate() error = %v", err)
	}
	if config.AOExecutable != "ao" || config.AOTimeout != DefaultAOTimeout || config.AOOutputLimit != DefaultAOOutputLimit {
		t.Fatalf("unexpected defaults: %#v", config)
	}
}

func TestValidateRejectsUnsafeOrAmbiguousConfiguration(t *testing.T) {
	repository, err := domain.ParseRepository("octo/repo")
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{AOExecutable: "/opt/bin/ao", AOTimeout: time.Second, AOOutputLimit: 1024, RepositoryProjects: []domain.RepositoryProject{{Repository: repository, AOProjectID: "octo-repo"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	cases := []Config{
		{AOExecutable: "", AOTimeout: time.Second, AOOutputLimit: 1},
		{AOExecutable: "ao", AOTimeout: 0, AOOutputLimit: 1},
		{AOExecutable: "ao", AOTimeout: time.Second, AOOutputLimit: 0},
		{AOExecutable: "ao", AOTimeout: time.Second, AOOutputLimit: 1, RepositoryProjects: []domain.RepositoryProject{{Repository: repository, AOProjectID: "p"}, {Repository: repository, AOProjectID: "p2"}}},
	}
	for _, config := range cases {
		if err := config.Validate(); err == nil {
			t.Errorf("Validate() succeeded for %#v", config)
		}
	}
}

func TestProjectFor(t *testing.T) {
	repository, _ := domain.ParseRepository("octo/repo")
	config := Defaults()
	config.RepositoryProjects = []domain.RepositoryProject{{Repository: repository, AOProjectID: "mapped-project"}}
	if got, ok := config.ProjectFor(repository); !ok || got != "mapped-project" {
		t.Fatalf("ProjectFor() = %q, %v", got, ok)
	}
}
