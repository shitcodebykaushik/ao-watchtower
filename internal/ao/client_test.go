package ao

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

type fakeProcess struct {
	executable string
	args       []string
	run        func(context.Context, io.Writer, io.Writer) error
}

func (f *fakeProcess) Run(ctx context.Context, executable string, args []string, stdout, stderr io.Writer) error {
	f.executable = executable
	f.args = append([]string(nil), args...)
	if f.run != nil {
		return f.run(ctx, stdout, stderr)
	}
	return nil
}

func newTestClient(t *testing.T, fake *fakeProcess) *Client {
	t.Helper()
	runner, err := NewRunner("/configured/ao", time.Second, 1024, fake)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(runner)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestClientBuildsDiscreteAOCommands(t *testing.T) {
	fake := &fakeProcess{run: func(_ context.Context, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, `{"projects":[{"id":"project-1","name":"Watchtower"}]}`)
		return nil
	}}
	client := newTestClient(t, fake)

	projects, err := client.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := projects, []Project{{ID: "project-1", Name: "Watchtower"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projects = %#v, want %#v", got, want)
	}
	assertCommand(t, fake, "/configured/ao", []string{"project", "ls", "--json"})

	fake.run = func(_ context.Context, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, `{"data":[{"id":"session-1","projectId":"project-1","status":"running"}],"meta":{"nextCursor":null}}`)
		return nil
	}
	sessions, err := client.ListLiveSessions(context.Background(), "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sessions, []Session{{ID: "session-1", ProjectID: "project-1", Status: "running"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions = %#v, want %#v", got, want)
	}
	assertCommand(t, fake, "/configured/ao", []string{"session", "ls", "--project", "project-1", "--json"})

	if _, err := client.SpawnInvestigator(context.Background(), InvestigatorRequest{ProjectID: "project-1", PullNumber: 42, Prompt: "Investigate CI only."}); err != nil {
		t.Fatal(err)
	}
	assertCommand(t, fake, "/configured/ao", []string{"spawn", "--project", "project-1", "--name", "ci-investigator", "--claim-pr", "42", "--no-takeover", "--harness", "codex", "--prompt", "Investigate CI only."})

	fake.run = func(_ context.Context, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, `{"session":{"id":"session-1","projectId":"project-1","status":"running"}}`)
		return nil
	}
	session, err := client.InspectSession(context.Background(), "project-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := session, (Session{ID: "session-1", ProjectID: "project-1", Status: "running"}); got != want {
		t.Fatalf("session = %#v, want %#v", got, want)
	}
	assertCommand(t, fake, "/configured/ao", []string{"session", "get", "session-1", "--project", "project-1", "--json"})

	if _, err := client.SendApprovedFollowup(context.Background(), "session-1", "Human approved a scoped fix."); err != nil {
		t.Fatal(err)
	}
	assertCommand(t, fake, "/configured/ao", []string{"send", "--session", "session-1", "--message", "Human approved a scoped fix."})
}

func TestRunnerReturnsTypedTimeoutAndBoundedOutput(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		fake := &fakeProcess{run: func(ctx context.Context, _, _ io.Writer) error { <-ctx.Done(); return ctx.Err() }}
		runner, err := NewRunner("ao", time.Millisecond, 10, fake)
		if err != nil {
			t.Fatal(err)
		}
		_, err = runner.Run(context.Background(), "project", "ls")
		var commandError *CommandError
		if !errors.As(err, &commandError) || commandError.Kind != ErrorTimeout {
			t.Fatalf("error = %#v, want timeout CommandError", err)
		}
	})

	t.Run("output limit", func(t *testing.T) {
		fake := &fakeProcess{run: func(_ context.Context, stdout, stderr io.Writer) error {
			_, _ = io.WriteString(stdout, "123456")
			_, _ = io.WriteString(stderr, "abcdef")
			return nil
		}}
		runner, err := NewRunner("ao", time.Second, 4, fake)
		if err != nil {
			t.Fatal(err)
		}
		result, err := runner.Run(context.Background(), "project", "ls")
		var commandError *CommandError
		if !errors.As(err, &commandError) || commandError.Kind != ErrorOutputLimit {
			t.Fatalf("error = %#v, want output-limit CommandError", err)
		}
		if got, want := string(result.Stdout), "1234"; got != want || string(result.Stderr) != "abcd" {
			t.Fatalf("bounded result = %#v", result)
		}
	})
}

func assertCommand(t *testing.T, fake *fakeProcess, executable string, want []string) {
	t.Helper()
	if fake.executable != executable || !reflect.DeepEqual(fake.args, want) {
		t.Fatalf("command = %q %#v, want %q %#v", fake.executable, fake.args, executable, want)
	}
}
