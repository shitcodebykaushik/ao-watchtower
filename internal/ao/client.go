package ao

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const investigatorName = "ci-investigator"

// Project is the narrow subset of an AO project that Watchtower requires.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Session is the narrow subset of an AO session used for ownership and audit links.
type Session struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Status    string `json:"status"`
}

// InvestigatorRequest fixes the initial workflow to a Codex CI investigator.
type InvestigatorRequest struct {
	ProjectID  string
	PullNumber int64
	Prompt     string
}

func (r InvestigatorRequest) validate() error {
	if strings.TrimSpace(r.ProjectID) == "" {
		return fmt.Errorf("AO project id is required")
	}
	if r.PullNumber <= 0 {
		return fmt.Errorf("pull number must be positive")
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return fmt.Errorf("investigator prompt is required")
	}
	return nil
}

// Client exposes only the product operations described in the MVP contract.
type Client struct{ runner *Runner }

func NewClient(runner *Runner) (*Client, error) {
	if runner == nil {
		return nil, fmt.Errorf("AO runner is required")
	}
	return &Client{runner: runner}, nil
}

func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var projects []Project
	if err := c.runJSON(ctx, &projects, "project", "ls", "--json"); err != nil {
		return nil, err
	}
	for index, project := range projects {
		if strings.TrimSpace(project.ID) == "" {
			return nil, fmt.Errorf("AO project %d has no id", index)
		}
	}
	return projects, nil
}

func (c *Client) ListLiveSessions(ctx context.Context, projectID string) ([]Session, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("AO project id is required")
	}
	var sessions []Session
	if err := c.runJSON(ctx, &sessions, "session", "ls", "--project", projectID, "--json"); err != nil {
		return nil, err
	}
	for index, session := range sessions {
		if strings.TrimSpace(session.ID) == "" {
			return nil, fmt.Errorf("AO session %d has no id", index)
		}
	}
	return sessions, nil
}

func (c *Client) SpawnInvestigator(ctx context.Context, request InvestigatorRequest) (CommandResult, error) {
	if err := request.validate(); err != nil {
		return CommandResult{}, err
	}
	return c.runner.Run(ctx,
		"spawn", "--project", request.ProjectID,
		"--name", investigatorName,
		"--claim-pr", strconv.FormatInt(request.PullNumber, 10),
		"--no-takeover", "--harness", "codex", "--prompt", request.Prompt,
	)
}

func (c *Client) InspectSession(ctx context.Context, projectID, sessionID string) (Session, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(sessionID) == "" {
		return Session{}, fmt.Errorf("AO project and session ids are required")
	}
	var session Session
	if err := c.runJSON(ctx, &session, "session", "get", sessionID, "--project", projectID, "--json"); err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(session.ID) == "" {
		return Session{}, fmt.Errorf("AO session has no id")
	}
	return session, nil
}

// SendApprovedFollowup deliberately accepts only a caller that has already checked durable approval.
func (c *Client) SendApprovedFollowup(ctx context.Context, sessionID, message string) (CommandResult, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(message) == "" {
		return CommandResult{}, fmt.Errorf("AO session id and message are required")
	}
	return c.runner.Run(ctx, "send", "--session", sessionID, "--message", message)
}

func (c *Client) runJSON(ctx context.Context, target any, args ...string) error {
	result, err := c.runner.Run(ctx, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(result.Stdout, target); err != nil {
		return fmt.Errorf("decode AO JSON: %w", err)
	}
	return nil
}
