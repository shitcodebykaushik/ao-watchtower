// Package config defines explicit Watchtower configuration and validation.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
)

const (
	DefaultAOTimeout     = 30 * time.Second
	DefaultAOOutputLimit = 1 << 20
)

// Config contains only explicit runtime controls needed by the foundation slice.
type Config struct {
	AOExecutable       string                     `json:"aoExecutable"`
	AOTimeout          time.Duration              `json:"aoTimeout"`
	AOOutputLimit      int                        `json:"aoOutputLimit"`
	RepositoryProjects []domain.RepositoryProject `json:"repositoryProjects"`
}

func Defaults() Config {
	return Config{AOExecutable: "ao", AOTimeout: DefaultAOTimeout, AOOutputLimit: DefaultAOOutputLimit}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.AOExecutable) == "" || strings.TrimSpace(c.AOExecutable) != c.AOExecutable {
		return fmt.Errorf("AO executable is required")
	}
	if c.AOTimeout <= 0 {
		return fmt.Errorf("AO timeout must be positive")
	}
	if c.AOOutputLimit <= 0 {
		return fmt.Errorf("AO output limit must be positive")
	}
	seen := make(map[domain.Repository]struct{}, len(c.RepositoryProjects))
	for index, mapping := range c.RepositoryProjects {
		if err := mapping.Validate(); err != nil {
			return fmt.Errorf("repository mapping %d: %w", index, err)
		}
		if _, exists := seen[mapping.Repository]; exists {
			return fmt.Errorf("repository mapping %d duplicates %s", index, mapping.Repository)
		}
		seen[mapping.Repository] = struct{}{}
	}
	return nil
}

// ProjectFor returns the configured AO project for a normalized repository.
func (c Config) ProjectFor(repository domain.Repository) (string, bool) {
	for _, mapping := range c.RepositoryProjects {
		if mapping.Repository == repository {
			return mapping.AOProjectID, true
		}
	}
	return "", false
}
