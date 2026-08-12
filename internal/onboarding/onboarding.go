// Package onboarding turns a checked-out GitHub repository into a private,
// local Watchtower installation using only the public git, gh, and ao CLIs.
package onboarding

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/config"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

const stateVersion = 1

// Commander is deliberately argv-only and injectable for offline tests.
type Commander interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecCommander struct{}

func (ExecCommander) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 2048 {
			message = message[:2048]
		}
		if message == "" {
			return nil, fmt.Errorf("run %s: %w", executable, err)
		}
		return nil, fmt.Errorf("run %s: %s", executable, message)
	}
	if stdout.Len() > 1<<20 {
		return nil, fmt.Errorf("run %s: output exceeds limit", executable)
	}
	return stdout.Bytes(), nil
}

type State struct {
	Version         int               `json:"version"`
	Repository      domain.Repository `json:"repository"`
	RepositoryPath  string            `json:"repositoryPath"`
	AOProjectID     string            `json:"aoProjectId"`
	AOExecutable    string            `json:"aoExecutable"`
	GHExecutable    string            `json:"ghExecutable"`
	Listen          string            `json:"listen"`
	SQLitePath      string            `json:"sqlitePath"`
	WebhookSecret   string            `json:"webhookSecret"`
	CallbackSecret  string            `json:"callbackSecret"`
	AdminToken      string            `json:"adminToken"`
	CallbackBaseURL string            `json:"callbackBaseURL"`
	CreatedAt       time.Time         `json:"createdAt"`
}

type Options struct {
	RepositoryPath string
	StateDirectory string
	AOExecutable   string
	GHExecutable   string
	Listen         string
	OverrideListen bool
	Commander      Commander
}

func Setup(ctx context.Context, options Options) (State, string, bool, error) {
	if options.Commander == nil {
		options.Commander = ExecCommander{}
	}
	if options.AOExecutable == "" {
		options.AOExecutable = "ao"
	}
	if options.GHExecutable == "" {
		options.GHExecutable = "gh"
	}
	if options.Listen == "" {
		options.Listen = config.DefaultListen
	}
	repository, root, err := DiscoverRepository(ctx, options.Commander, options.RepositoryPath)
	if err != nil {
		return State{}, "", false, err
	}
	directory := options.StateDirectory
	if directory == "" {
		directory, err = DefaultStateDirectory(repository)
		if err != nil {
			return State{}, "", false, err
		}
	}
	statePath := filepath.Join(directory, "state.json")
	if state, err := Load(statePath); err == nil {
		if state.Repository != repository || !samePath(state.RepositoryPath, root) {
			return State{}, statePath, false, fmt.Errorf("state belongs to %s at %s", state.Repository, state.RepositoryPath)
		}
		if err := verifyExisting(ctx, options.Commander, state); err != nil {
			return State{}, statePath, false, err
		}
		if options.OverrideListen && state.Listen != options.Listen {
			state.Listen = options.Listen
			state.CallbackBaseURL = "http://" + options.Listen
			if err := Save(statePath, state); err != nil {
				return State{}, statePath, false, err
			}
		}
		return state, statePath, false, nil
	} else if !os.IsNotExist(err) {
		return State{}, statePath, false, err
	}

	if err := checkGH(ctx, options.Commander, options.GHExecutable); err != nil {
		return State{}, statePath, false, err
	}
	projectID, err := ensureAOProject(ctx, options.Commander, options.AOExecutable, repository, root)
	if err != nil {
		return State{}, statePath, false, err
	}
	webhookSecret, err := randomSecret()
	if err != nil {
		return State{}, statePath, false, err
	}
	callbackSecret, err := randomSecret()
	if err != nil {
		return State{}, statePath, false, err
	}
	adminToken, err := randomSecret()
	if err != nil {
		return State{}, statePath, false, err
	}
	state := State{
		Version: stateVersion, Repository: repository, RepositoryPath: root,
		AOProjectID: projectID, AOExecutable: options.AOExecutable, GHExecutable: options.GHExecutable,
		Listen: options.Listen, SQLitePath: filepath.Join(directory, "watchtower.db"),
		WebhookSecret: webhookSecret, CallbackSecret: callbackSecret, AdminToken: adminToken,
		CallbackBaseURL: "http://" + options.Listen, CreatedAt: time.Now().UTC(),
	}
	if err := Save(statePath, state); err != nil {
		return State{}, statePath, false, err
	}
	return state, statePath, true, nil
}

func (s State) Config() (config.Config, error) {
	c := config.Defaults()
	c.AOExecutable = s.AOExecutable
	c.Listen = s.Listen
	c.SQLitePath = s.SQLitePath
	c.WebhookSecret = s.WebhookSecret
	c.CallbackSecret = s.CallbackSecret
	c.AdminToken = s.AdminToken
	c.CallbackBaseURL = s.CallbackBaseURL
	c.RepositoryProjects = []domain.RepositoryProject{{Repository: s.Repository, AOProjectID: s.AOProjectID}}
	return c, c.ValidateRuntime()
}

func DefaultStateDirectory(repository domain.Repository) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(base, "ao-watchtower", repository.Owner, repository.Name), nil
}

func Save(path string, state State) error {
	if err := validateState(state); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("create state file: %w", err)
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return fmt.Errorf("protect state file: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync state file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install state file: %w", err)
	}
	ok = true
	return nil
}

func Load(path string) (State, error) {
	info, err := os.Stat(path)
	if err != nil {
		return State{}, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return State{}, fmt.Errorf("state file %s must not be accessible by group or others", path)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return State{}, fmt.Errorf("decode state: multiple JSON values")
	}
	if err := validateState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func validateState(state State) error {
	if state.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	if err := state.Repository.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(state.RepositoryPath) == "" || strings.TrimSpace(state.AOProjectID) == "" ||
		strings.TrimSpace(state.AOExecutable) == "" || strings.TrimSpace(state.GHExecutable) == "" ||
		strings.TrimSpace(state.Listen) == "" || strings.TrimSpace(state.SQLitePath) == "" ||
		state.WebhookSecret == "" || state.CallbackSecret == "" || state.AdminToken == "" || state.CallbackBaseURL == "" {
		return fmt.Errorf("state is incomplete")
	}
	return nil
}

func DiscoverRepository(ctx context.Context, commander Commander, requestedPath string) (domain.Repository, string, error) {
	if requestedPath == "" {
		requestedPath = "."
	}
	rootOutput, err := commander.Run(ctx, "git", "-C", requestedPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return domain.Repository{}, "", fmt.Errorf("discover git repository: %w", err)
	}
	root, err := filepath.Abs(strings.TrimSpace(string(rootOutput)))
	if err != nil || root == "" {
		return domain.Repository{}, "", fmt.Errorf("discover git repository: invalid root")
	}
	remoteOutput, err := commander.Run(ctx, "git", "-C", root, "remote", "get-url", "origin")
	if err != nil {
		return domain.Repository{}, "", fmt.Errorf("discover GitHub origin: %w", err)
	}
	repository, err := ParseGitHubRemote(strings.TrimSpace(string(remoteOutput)))
	if err != nil {
		return domain.Repository{}, "", err
	}
	return repository, filepath.Clean(root), nil
}

func ParseGitHubRemote(remote string) (domain.Repository, error) {
	value := strings.TrimSpace(remote)
	switch {
	case strings.HasPrefix(value, "https://github.com/"):
		value = strings.TrimPrefix(value, "https://github.com/")
	case strings.HasPrefix(value, "git@github.com:"):
		value = strings.TrimPrefix(value, "git@github.com:")
	case strings.HasPrefix(value, "ssh://git@github.com/"):
		value = strings.TrimPrefix(value, "ssh://git@github.com/")
	default:
		return domain.Repository{}, fmt.Errorf("origin must be a github.com HTTPS or SSH repository")
	}
	value = strings.TrimSuffix(value, ".git")
	repository, err := domain.ParseRepository(value)
	if err != nil {
		return domain.Repository{}, fmt.Errorf("parse GitHub origin: %w", err)
	}
	return repository, nil
}

func checkGH(ctx context.Context, commander Commander, executable string) error {
	if _, err := commander.Run(ctx, executable, "auth", "status"); err != nil {
		return fmt.Errorf("GitHub CLI is not authenticated; run `gh auth login`: %w", err)
	}
	return nil
}

func verifyExisting(ctx context.Context, commander Commander, state State) error {
	if err := checkGH(ctx, commander, state.GHExecutable); err != nil {
		return err
	}
	if err := checkAOReady(ctx, commander, state.AOExecutable); err != nil {
		return err
	}
	output, err := commander.Run(ctx, state.AOExecutable, "project", "get", state.AOProjectID, "--json")
	if err != nil {
		return fmt.Errorf("configured AO project %q is unavailable: %w", state.AOProjectID, err)
	}
	var response struct {
		Project struct {
			Path string `json:"path"`
		} `json:"project"`
	}
	if err := json.Unmarshal(output, &response); err != nil || !samePath(response.Project.Path, state.RepositoryPath) {
		return fmt.Errorf("configured AO project %q does not own %s", state.AOProjectID, state.RepositoryPath)
	}
	return nil
}

func checkAOReady(ctx context.Context, commander Commander, executable string) error {
	statusOutput, err := commander.Run(ctx, executable, "status", "--json")
	if err != nil {
		return fmt.Errorf("AO is not running; open AO or run `ao start`: %w", err)
	}
	var status struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(statusOutput, &status); err != nil || status.State != "ready" {
		return fmt.Errorf("AO is not ready")
	}
	return nil
}

func ensureAOProject(ctx context.Context, commander Commander, executable string, repository domain.Repository, root string) (string, error) {
	if err := checkAOReady(ctx, commander, executable); err != nil {
		return "", err
	}
	listOutput, err := commander.Run(ctx, executable, "project", "ls", "--json")
	if err != nil {
		return "", fmt.Errorf("list AO projects: %w", err)
	}
	var list struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(listOutput, &list); err != nil {
		return "", fmt.Errorf("decode AO projects: %w", err)
	}
	used := make(map[string]bool, len(list.Projects))
	for _, project := range list.Projects {
		used[project.ID] = true
		getOutput, getErr := commander.Run(ctx, executable, "project", "get", project.ID, "--json")
		if getErr != nil {
			continue
		}
		var response struct {
			Project struct {
				Path string `json:"path"`
			} `json:"project"`
		}
		if json.Unmarshal(getOutput, &response) == nil && response.Project.Path != "" && samePath(response.Project.Path, root) {
			return project.ID, nil
		}
	}
	base := safeProjectID(repository.Name)
	projectID := base
	for suffix := 2; used[projectID]; suffix++ {
		projectID = fmt.Sprintf("%s-%d", base, suffix)
	}
	_, err = commander.Run(ctx, executable, "project", "add", "--id", projectID, "--name", repository.String(), "--path", root, "--worker-agent", "codex", "--orchestrator-agent", "codex")
	if err != nil {
		return "", fmt.Errorf("register AO project: %w", err)
	}
	return projectID, nil
}

func safeProjectID(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			result.WriteRune(character)
		} else if result.Len() > 0 && !strings.HasSuffix(result.String(), "-") {
			result.WriteByte('-')
		}
	}
	value = strings.Trim(result.String(), "-")
	if value == "" {
		return "project"
	}
	return value
}

func samePath(first, second string) bool {
	firstAbs, firstErr := filepath.Abs(first)
	secondAbs, secondErr := filepath.Abs(second)
	return firstErr == nil && secondErr == nil && filepath.Clean(firstAbs) == filepath.Clean(secondAbs)
}

func randomSecret() (string, error) {
	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(payload), nil
}
