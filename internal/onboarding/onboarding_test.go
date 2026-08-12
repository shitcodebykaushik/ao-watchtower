package onboarding

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeCommander struct {
	t      *testing.T
	root   string
	calls  [][]string
	added  bool
	failOn string
}

func (f *fakeCommander) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	call := append([]string{executable}, args...)
	f.calls = append(f.calls, call)
	joined := strings.Join(call, " ")
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		f.t.Fatalf("unexpected command: %s", joined)
	}
	switch {
	case executable == "git" && reflect.DeepEqual(args[len(args)-2:], []string{"rev-parse", "--show-toplevel"}):
		return []byte(f.root + "\n"), nil
	case executable == "git" && reflect.DeepEqual(args[len(args)-3:], []string{"remote", "get-url", "origin"}):
		return []byte("git@github.com:Example/Service.git\n"), nil
	case executable == "gh" && reflect.DeepEqual(args, []string{"auth", "status"}):
		return nil, nil
	case executable == "ao" && reflect.DeepEqual(args, []string{"status", "--json"}):
		return []byte(`{"state":"ready"}`), nil
	case executable == "ao" && reflect.DeepEqual(args, []string{"project", "ls", "--json"}):
		return []byte(`{"projects":[]}`), nil
	case executable == "ao" && reflect.DeepEqual(args, []string{"project", "get", "service", "--json"}):
		return []byte(`{"project":{"path":"` + f.root + `"}}`), nil
	case executable == "ao" && len(args) > 2 && args[0] == "project" && args[1] == "add":
		f.added = true
		return []byte("registered project service\n"), nil
	default:
		f.t.Fatalf("unexpected command: %s", joined)
		return nil, nil
	}
}

func TestParseGitHubRemote(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/Owner/Repo.git",
		"git@github.com:Owner/Repo.git",
		"ssh://git@github.com/Owner/Repo",
	} {
		repository, err := ParseGitHubRemote(remote)
		if err != nil || repository.String() != "owner/repo" {
			t.Fatalf("remote=%q repository=%s err=%v", remote, repository, err)
		}
	}
	if _, err := ParseGitHubRemote("https://example.com/owner/repo"); err == nil {
		t.Fatal("expected non-GitHub remote rejection")
	}
}

func TestSetupCreatesProtectedReusableState(t *testing.T) {
	root := t.TempDir()
	stateDirectory := filepath.Join(t.TempDir(), "state")
	commands := &fakeCommander{t: t, root: root}
	state, path, created, err := Setup(context.Background(), Options{
		RepositoryPath: root, StateDirectory: stateDirectory,
		AOExecutable: "ao", GHExecutable: "gh", Commander: commands,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || !commands.added || state.Repository.String() != "example/service" || state.AOProjectID != "service" {
		t.Fatalf("state=%#v created=%t added=%t", state, created, commands.added)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode=%o", info.Mode().Perm())
	}
	configuration, err := state.Config()
	if err != nil || configuration.AdminToken == "" || configuration.WebhookSecret == configuration.CallbackSecret {
		t.Fatalf("configuration=%#v err=%v", configuration, err)
	}

	commands.added = false
	reused, reusedPath, created, err := Setup(context.Background(), Options{
		RepositoryPath: root, StateDirectory: stateDirectory,
		AOExecutable: "ao", GHExecutable: "gh", Commander: commands,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || reusedPath != path || reused.AdminToken != state.AdminToken || commands.added {
		t.Fatalf("reused=%#v path=%s created=%t", reused, reusedPath, created)
	}
}

func TestLoadRejectsExposedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("err=%v", err)
	}
}
