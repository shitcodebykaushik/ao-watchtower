// Package config defines explicit Watchtower configuration and validation.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
)

const (
	DefaultAOTimeout     = 30 * time.Second
	DefaultAOOutputLimit = 1 << 20
	DefaultListen        = "127.0.0.1:8787"
)

type Config struct {
	AOExecutable       string                     `json:"aoExecutable"`
	AOTimeout          time.Duration              `json:"aoTimeout"`
	AOOutputLimit      int                        `json:"aoOutputLimit"`
	RepositoryProjects []domain.RepositoryProject `json:"repositoryProjects"`
	Listen             string                     `json:"listen"`
	SQLitePath         string                     `json:"sqlitePath"`
	WebhookSecret      string                     `json:"-"`
	AdminToken         string                     `json:"-"`
	CallbackBaseURL    string                     `json:"callbackBaseURL"`
	CallbackSecret     string                     `json:"-"`
	Demo               bool                       `json:"-"`
}

func Defaults() Config {
	return Config{AOExecutable: "ao", AOTimeout: DefaultAOTimeout, AOOutputLimit: DefaultAOOutputLimit, Listen: DefaultListen, SQLitePath: "watchtower.db"}
}

// Load combines safe defaults, an optional JSON file, and explicit WT_* environment overrides. Secrets are environment-only.
func Load(file string) (Config, error) {
	c := Defaults()
	if file != "" {
		b, e := os.ReadFile(file)
		if e != nil {
			return c, fmt.Errorf("read config: %w", e)
		}
		var raw struct {
			AOExecutable       string                     `json:"aoExecutable"`
			AOTimeout          string                     `json:"aoTimeout"`
			AOOutputLimit      int                        `json:"aoOutputLimit"`
			RepositoryProjects []domain.RepositoryProject `json:"repositoryProjects"`
			Listen             string                     `json:"listen"`
			SQLitePath         string                     `json:"sqlitePath"`
			CallbackBaseURL    string                     `json:"callbackBaseURL"`
		}
		if e = json.Unmarshal(b, &raw); e != nil {
			return c, fmt.Errorf("parse config: %w", e)
		}
		if raw.AOExecutable != "" {
			c.AOExecutable = raw.AOExecutable
		}
		if raw.AOTimeout != "" {
			c.AOTimeout, e = time.ParseDuration(raw.AOTimeout)
			if e != nil {
				return c, fmt.Errorf("parse aoTimeout: %w", e)
			}
		}
		if raw.AOOutputLimit != 0 {
			c.AOOutputLimit = raw.AOOutputLimit
		}
		if raw.RepositoryProjects != nil {
			c.RepositoryProjects = raw.RepositoryProjects
		}
		if raw.Listen != "" {
			c.Listen = raw.Listen
		}
		if raw.SQLitePath != "" {
			c.SQLitePath = raw.SQLitePath
		}
		if raw.CallbackBaseURL != "" {
			c.CallbackBaseURL = raw.CallbackBaseURL
		}
	}
	set := func(k string, p *string) {
		if v, ok := os.LookupEnv(k); ok {
			*p = v
		}
	}
	set("WT_LISTEN", &c.Listen)
	set("WT_SQLITE_PATH", &c.SQLitePath)
	set("WT_AO_EXECUTABLE", &c.AOExecutable)
	set("WT_WEBHOOK_SECRET", &c.WebhookSecret)
	set("WT_ADMIN_TOKEN", &c.AdminToken)
	set("WT_CALLBACK_BASE_URL", &c.CallbackBaseURL)
	set("WT_CALLBACK_SECRET", &c.CallbackSecret)
	return c, c.ValidateRuntime()
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
	seen := map[domain.Repository]struct{}{}
	for i, m := range c.RepositoryProjects {
		if e := m.Validate(); e != nil {
			return fmt.Errorf("repository mapping %d: %w", i, e)
		}
		if _, ok := seen[m.Repository]; ok {
			return fmt.Errorf("repository mapping %d duplicates %s", i, m.Repository)
		}
		seen[m.Repository] = struct{}{}
	}
	return nil
}
func (c Config) ValidateRuntime() error {
	if e := c.Validate(); e != nil {
		return e
	}
	if strings.TrimSpace(c.Listen) == "" || strings.TrimSpace(c.SQLitePath) == "" {
		return fmt.Errorf("listen address and SQLite path are required")
	}
	if c.CallbackBaseURL != "" {
		u, err := url.Parse(c.CallbackBaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("callback base URL must be an absolute HTTP(S) URL without credentials or fragments")
		}
	}
	if !c.Demo && (c.WebhookSecret == "" || c.AdminToken == "" || c.CallbackSecret == "" || c.CallbackBaseURL == "" || len(c.RepositoryProjects) == 0) {
		return fmt.Errorf("webhook secret, admin token, callback base URL/secret, and repository mapping are required")
	}
	return nil
}
func (c Config) ProjectFor(r domain.Repository) (string, bool) {
	for _, m := range c.RepositoryProjects {
		if m.Repository == r {
			return m.AOProjectID, true
		}
	}
	return "", false
}
