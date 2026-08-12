package ao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// projectListResponse is the AO project ls JSON envelope.
type projectListResponse struct {
	Projects []Project `json:"projects"`
}

// sessionListResponse is the AO session ls JSON envelope. Meta is intentionally
// omitted because Watchtower needs only the live-session records.
type sessionListResponse struct {
	Data []Session `json:"data"`
}

// sessionGetResponse is the AO session get JSON envelope.
type sessionGetResponse struct {
	Session *Session `json:"session"`
}

var stableClaimConflictCode = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])PR_CLAIMED_BY_ACTIVE_SESSION(?:$|[^A-Za-z0-9_])`)

const maxSpawnSuffix = 4096

// ClaimConflictError means AO refused --claim-pr because another worker owns
// the pull request. It is distinct from a generic command failure so callers
// can audit the event as linked/skipped and, critically, never retry with a
// takeover.
type ClaimConflictError struct{ Cause error }

func (e *ClaimConflictError) Error() string { return "AO pull request claim conflict" }
func (e *ClaimConflictError) Unwrap() error { return e.Cause }

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
	var response projectListResponse
	if err := c.runJSON(ctx, &response, "project", "ls", "--json"); err != nil {
		return nil, err
	}
	if response.Projects == nil {
		return nil, fmt.Errorf("decode AO JSON: missing projects envelope")
	}
	for index, project := range response.Projects {
		if strings.TrimSpace(project.ID) == "" {
			return nil, fmt.Errorf("AO project %d has no id", index)
		}
	}
	return response.Projects, nil
}

func (c *Client) ListLiveSessions(ctx context.Context, projectID string) ([]Session, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("AO project id is required")
	}
	var response sessionListResponse
	if err := c.runJSON(ctx, &response, "session", "ls", "--project", projectID, "--json"); err != nil {
		return nil, err
	}
	if response.Data == nil {
		return nil, fmt.Errorf("decode AO JSON: missing session data envelope")
	}
	for index, session := range response.Data {
		if strings.TrimSpace(session.ID) == "" {
			return nil, fmt.Errorf("AO session %d has no id", index)
		}
	}
	return response.Data, nil
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

// SpawnInvestigatorSession performs the one allowed ownership-and-spawn
// mutation. AO's --claim-pr --no-takeover flags are the sole ownership
// authority; Watchtower never infers ownership from session listings.
func (c *Client) SpawnInvestigatorSession(ctx context.Context, request InvestigatorRequest) (Session, error) {
	if err := request.validate(); err != nil {
		return Session{}, err
	}
	result, err := c.runner.Run(ctx,
		"spawn", "--project", request.ProjectID,
		"--name", investigatorName,
		"--claim-pr", strconv.FormatInt(request.PullNumber, 10),
		"--no-takeover", "--harness", "codex", "--prompt", request.Prompt,
	)
	if err != nil {
		if isClaimConflict(result) {
			return Session{}, &ClaimConflictError{Cause: err}
		}
		return Session{}, err
	}
	sessionID, status, err := parseSpawnedSession(result.Stdout)
	if err != nil {
		return Session{}, err
	}
	return Session{ID: sessionID, ProjectID: request.ProjectID, Status: status}, nil
}

// parseSpawnedSession accepts AO's documented text success line and no other
// output shape. The output is already bounded by Runner; only a safe session
// token is retained for durable linking.
func parseSpawnedSession(output []byte) (string, string, error) {
	line := strings.TrimSuffix(string(output), "\n")
	line = strings.TrimSuffix(line, "\r")
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return "", "", fmt.Errorf("parse AO spawn output: expected one spawned-session line")
	}
	const prefix = "spawned session "
	if !strings.HasPrefix(line, prefix) {
		return "", "", fmt.Errorf("parse AO spawn output: missing spawned-session line")
	}
	remainder := strings.TrimPrefix(line, prefix)
	separator := strings.Index(remainder, " (")
	if separator <= 0 {
		return "", "", fmt.Errorf("parse AO spawn output: invalid session line")
	}
	sessionID := remainder[:separator]
	afterID := remainder[separator+2:]
	closing := strings.IndexByte(afterID, ')')
	if closing <= 0 || !safeSessionToken(sessionID) {
		return "", "", fmt.Errorf("parse AO spawn output: invalid session id")
	}
	status := afterID[:closing]
	if !safeSessionToken(status) {
		return "", "", fmt.Errorf("parse AO spawn output: invalid session status")
	}
	if suffix := afterID[closing+1:]; len(suffix) > maxSpawnSuffix {
		return "", "", fmt.Errorf("parse AO spawn output: suffix exceeds limit")
	}
	return sessionID, status, nil
}

func safeSessionToken(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

// isClaimConflict recognizes only AO's stable no-takeover conflict code. AO
// emits surrounding rollback prose, which is deliberately ignored.
func isClaimConflict(result CommandResult) bool {
	for _, source := range [][]byte{result.Stdout, result.Stderr} {
		if stableClaimConflictCode.Match(source) {
			return true
		}
	}
	return false
}

// IsClaimConflict lets the application classify a typed AO failure without
// depending on process output or error strings.
func IsClaimConflict(err error) bool {
	var conflict *ClaimConflictError
	return errors.As(err, &conflict)
}

func (c *Client) InspectSession(ctx context.Context, projectID, sessionID string) (Session, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(sessionID) == "" {
		return Session{}, fmt.Errorf("AO project and session ids are required")
	}
	var response sessionGetResponse
	if err := c.runJSON(ctx, &response, "session", "get", sessionID, "--project", projectID, "--json"); err != nil {
		return Session{}, err
	}
	if response.Session == nil || strings.TrimSpace(response.Session.ID) == "" {
		return Session{}, fmt.Errorf("AO session has no id")
	}
	return *response.Session, nil
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
