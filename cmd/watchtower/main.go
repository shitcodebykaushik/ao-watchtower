// Command watchtower runs the local AO Watchtower MVP.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ao"
	"github.com/shitcodebykaushik/ao-watchtower/internal/automation"
	"github.com/shitcodebykaushik/ao-watchtower/internal/config"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/intake"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/notify"
	"github.com/shitcodebykaushik/ao-watchtower/internal/onboarding"
	"github.com/shitcodebykaushik/ao-watchtower/internal/polling"
	"github.com/shitcodebykaushik/ao-watchtower/internal/prcomment"
	"github.com/shitcodebykaushik/ao-watchtower/internal/repopolicy"
	"github.com/shitcodebykaushik/ao-watchtower/internal/scheduler"
	"github.com/shitcodebykaushik/ao-watchtower/internal/service"
	"github.com/shitcodebykaushik/ao-watchtower/internal/verification"
	"github.com/shitcodebykaushik/ao-watchtower/internal/web"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Version is the build identity, overridable at link time with
// -ldflags "-X main.Version=v1.2.3". A binary that cannot say what it is makes
// an audit trail hard to trust.
var Version = "dev"

type demoAO struct{}

func (demoAO) SpawnInvestigatorSession(_ context.Context, r ao.InvestigatorRequest) (ao.Session, error) {
	return ao.Session{ID: "demo-session", ProjectID: r.ProjectID, Status: "running"}, nil
}
func (demoAO) SendApprovedFollowup(context.Context, string, string) (ao.CommandResult, error) {
	return ao.CommandResult{}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runCLI(ctx, os.Args[1:], os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func runCLI(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || args[0] == "serve" {
		if len(args) > 0 && args[0] == "serve" {
			args = args[1:]
		}
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		flags.SetOutput(output)
		file := flags.String("config", os.Getenv("WT_CONFIG"), "JSON configuration file")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: watchtower serve [-config file]")
		}
		configuration, err := config.Load(*file)
		if err != nil {
			return err
		}
		return serve(ctx, configuration, runtimeOptions{}, nil)
	}
	switch args[0] {
	case "demo":
		if len(args) != 1 {
			return fmt.Errorf("usage: watchtower demo")
		}
		configuration := demoConfig()
		return serve(ctx, configuration, runtimeOptions{}, func(_ context.Context, _ http.Handler, address string, _ *ledger.Ledger, _ *service.Lifecycle) error {
			go demoDelivery("http://"+address+"/webhooks/github", configuration.WebhookSecret)
			return nil
		})
	case "init":
		return runInit(ctx, args[1:], output, false)
	case "up":
		return runInit(ctx, args[1:], output, true)
	case "status":
		return runStatus(ctx, args[1:], output)
	case "stats":
		return runStats(ctx, args[1:], output)
	case "version", "--version":
		fmt.Fprintf(output, "watchtower %s\n", Version)
		return nil
	case "help", "-h", "--help":
		printUsage(output)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run `watchtower help`", args[0])
	}
}

func runInit(ctx context.Context, args []string, output io.Writer, start bool) error {
	name := "init"
	if start {
		name = "up"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	repositoryPath := flags.String("repo", ".", "path inside the Git repository")
	stateDirectory := flags.String("state-dir", "", "private state directory (advanced)")
	aoExecutable := flags.String("ao", os.Getenv("WT_AO_EXECUTABLE"), "AO executable (auto-detected by default)")
	ghExecutable := flags.String("gh", "gh", "GitHub CLI executable")
	listen := flags.String("listen", config.DefaultListen, "dashboard listen address")
	pollInterval := flags.Duration("poll-interval", 5*time.Second, "GitHub check interval")
	autoFix := flags.Bool("auto-fix", false, "automatically approve and publish high-confidence code fixes")
	autoFixActor := flags.String("auto-fix-actor", "local-auto-fix", "approval identity used by --auto-fix")
	minimumConfidence := flags.Float64("auto-fix-confidence", automation.DefaultMinimumConfidence, "minimum confidence for --auto-fix")
	comment := flags.Bool("comment", false, "publish the diagnosis and repair outcome as a pull request comment")
	maxConcurrent := flags.Int("max-investigations", 3, "maximum concurrent AO investigator sessions; 0 means unlimited")
	dailyFixBudget := flags.Int("daily-fix-budget", 20, "maximum automatic fixes dispatched per rolling 24 hours; 0 means unlimited")
	verifyTimeout := flags.Duration("verify-timeout", verification.DefaultTimeout, "how long to watch a dispatched fix before recording it as unverified")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *maxConcurrent < 0 || *dailyFixBudget < 0 {
		return fmt.Errorf("--max-investigations and --daily-fix-budget must not be negative")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: watchtower %s [options]", name)
	}
	listenWasSet := false
	flags.Visit(func(selected *flag.Flag) {
		if selected.Name == "listen" {
			listenWasSet = true
		}
	})
	resolvedAO, err := resolveAOExecutable(*aoExecutable)
	if err != nil {
		return err
	}
	setupContext, cancel := context.WithTimeout(ctx, 90*time.Second)
	state, statePath, created, err := onboarding.Setup(setupContext, onboarding.Options{
		RepositoryPath: *repositoryPath, StateDirectory: *stateDirectory,
		AOExecutable: resolvedAO, GHExecutable: *ghExecutable, Listen: *listen, OverrideListen: listenWasSet,
	})
	cancel()
	if err != nil {
		return err
	}
	verb := "Reused"
	if created {
		verb = "Created"
	}
	fmt.Fprintf(output, "%s Watchtower setup for %s\n", verb, state.Repository)
	fmt.Fprintf(output, "AO project: %s\nState: %s\n", state.AOProjectID, statePath)
	if !start {
		fmt.Fprintln(output, "Run `watchtower up` from this repository to start monitoring.")
		return nil
	}
	configuration, err := state.Config()
	if err != nil {
		return err
	}
	githubClient, err := polling.NewClient(state.GHExecutable, nil)
	if err != nil {
		return err
	}
	// A committed repository policy is loaded before anything starts. A policy
	// that exists but cannot be parsed is fatal: an owner who wrote one must
	// never be silently downgraded to permissive automation.
	repositoryPolicy, err := repopolicy.Load(state.RepositoryPath)
	if err != nil {
		return err
	}
	mode := "review"
	if *autoFix {
		mode = "auto-fix"
	}
	fmt.Fprintf(output, "Dashboard: http://%s\n", state.Listen)
	fmt.Fprintf(output, "Admin token: %s\n", state.AdminToken)
	if *autoFix {
		fmt.Fprintf(output, "Mode: AUTO-FIX (code diagnoses at or above %.0f%%)\n", *minimumConfidence*100)
		fmt.Fprintf(output, "Limits: %s concurrent investigations, %s fixes per day\n", limitLabel(*maxConcurrent), limitLabel(*dailyFixBudget))
	} else {
		fmt.Fprintln(output, "Mode: REVIEW (approve fixes in the dashboard; use --auto-fix for hands-free repair)")
		fmt.Fprintf(output, "Limits: %s concurrent investigations\n", limitLabel(*maxConcurrent))
	}
	if repositoryPolicy.AutoFix.Constrains() {
		fmt.Fprintf(output, "Repository policy: %s applies\n", repopolicy.PolicyFileName)
	}
	if *comment {
		fmt.Fprintln(output, "Pull request comments: enabled")
	}
	fmt.Fprintf(output, "Monitoring GitHub every %s; keep this command running. Press Ctrl+C to stop.\n", pollInterval.String())
	options := runtimeOptions{MaxConcurrentInvestigations: *maxConcurrent}
	return serve(ctx, configuration, options, func(runContext context.Context, handler http.Handler, _ string, durable *ledger.Ledger, lifecycle *service.Lifecycle) error {
		var notifier *notify.Controller
		if *comment {
			publisher, err := prcomment.NewPublisher(state.GHExecutable, nil)
			if err != nil {
				return err
			}
			notifier, err = notify.New(durable, publisher, mode, notify.Options{Logger: log.Default()})
			if err != nil {
				return err
			}
			go notifier.Run(runContext)
		}
		recorderOptions := verification.Options{Timeout: *verifyTimeout, Logger: log.Default()}
		if notifier != nil {
			recorderOptions.Notifier = notifier
		}
		recorder, err := verification.NewRecorder(durable, recorderOptions)
		if err != nil {
			return err
		}
		poller, err := polling.New(githubClient, state.Repository, []byte(state.WebhookSecret), handler, *pollInterval, log.Default(), polling.Options{Observer: recorder})
		if err != nil {
			return err
		}
		go poller.Run(runContext)
		// The scheduler replays reservations that capacity or a restart left
		// without a spawn attempt.
		backlog, err := scheduler.New(durable, lifecycle, scheduler.Options{Interval: *pollInterval, Logger: log.Default()})
		if err != nil {
			return err
		}
		go backlog.Run(runContext)
		if *autoFix {
			controller, err := automation.New(durable, lifecycle, *autoFixActor, *minimumConfidence, time.Second, log.Default(),
				automation.Options{DailyFixBudget: *dailyFixBudget, Gate: policyGate{policy: repositoryPolicy, floor: *minimumConfidence}})
			if err != nil {
				return err
			}
			go controller.Run(runContext)
		}
		return nil
	})
}

// policyGate adapts the committed repository policy to the automation gate.
type policyGate struct {
	policy repopolicy.Policy
	floor  float64
}

func (g policyGate) AllowAutoFix(diagnosis domain.Diagnosis) (bool, string) {
	decision := g.policy.EvaluateAutoFix(diagnosis, g.floor)
	if decision.Allowed {
		return true, decision.Reason
	}
	return false, decision.Reason + ": " + decision.Detail
}

func limitLabel(limit int) string {
	if limit <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", limit)
}

func resolveAOExecutable(requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	if path, err := exec.LookPath(aoExecutableName); err == nil {
		return path, nil
	}
	for _, candidate := range aoInstallCandidates() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && isExecutableMode(info) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot find AO; open Agent Orchestrator or pass --ao /path/to/ao")
}

func runStatus(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(output)
	repositoryPath := flags.String("repo", ".", "path inside the Git repository")
	stateDirectory := flags.String("state-dir", "", "private state directory (advanced)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: watchtower status [options]")
	}
	state, err := loadState(ctx, *repositoryPath, *stateDirectory)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+state.Listen+"/healthz", nil)
	response, healthErr := client.Do(request)
	running := healthErr == nil && response.StatusCode == http.StatusOK
	if response != nil {
		_ = response.Body.Close()
	}
	fmt.Fprintf(output, "Repository: %s\nAO project: %s\nDashboard: http://%s\nRunning: %t\n", state.Repository, state.AOProjectID, state.Listen, running)
	return nil
}

// runStats prints the durable outcome record for a repository. It answers the
// question a dashboard glance cannot: over time, is this automation actually
// turning failed pull requests green?
func runStats(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("stats", flag.ContinueOnError)
	flags.SetOutput(output)
	repositoryPath := flags.String("repo", ".", "path inside the Git repository")
	stateDirectory := flags.String("state-dir", "", "private state directory (advanced)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: watchtower stats [options]")
	}
	state, err := loadState(ctx, *repositoryPath, *stateDirectory)
	if err != nil {
		return err
	}
	durable, err := ledger.Open(state.SQLitePath)
	if err != nil {
		return err
	}
	defer durable.Close()
	stats, err := durable.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Repository: %s\n", state.Repository)
	fmt.Fprintf(output, "Failures seen:        %d\n", stats.Triggers)
	fmt.Fprintf(output, "Investigated:         %d (%d claim conflicts)\n", stats.Spawned, stats.ClaimConflicts)
	fmt.Fprintf(output, "Valid diagnoses:      %d\n", stats.ValidDiagnoses)
	fmt.Fprintf(output, "Approved:             %d\n", stats.Approvals)
	fmt.Fprintf(output, "Fixes dispatched:     %d\n", stats.Dispatched)
	fmt.Fprintf(output, "Verified green:       %d\n", stats.VerifiedGreen)
	fmt.Fprintf(output, "Still failing:        %d\n", stats.StillFailing)
	fmt.Fprintf(output, "Unverified:           %d (%d awaiting)\n", stats.Abandoned, stats.AwaitingVerification)
	if settled := stats.VerifiedGreen + stats.StillFailing; settled > 0 {
		fmt.Fprintf(output, "Repair success rate:  %.0f%%\n", stats.RepairSuccessRate()*100)
	}
	if stats.MedianTimeToGreen > 0 {
		fmt.Fprintf(output, "Median time to green: %s\n", stats.MedianTimeToGreen.Round(time.Second))
	}
	if stats.AutomationDisabled {
		fmt.Fprintln(output, "Automation:           KILL SWITCH ENABLED")
	}
	return nil
}

// loadState resolves the private installation state for a checkout.
func loadState(ctx context.Context, repositoryPath, stateDirectory string) (onboarding.State, error) {
	repository, _, err := onboarding.DiscoverRepository(ctx, onboarding.ExecCommander{}, repositoryPath)
	if err != nil {
		return onboarding.State{}, err
	}
	directory := stateDirectory
	if directory == "" {
		directory, err = onboarding.DefaultStateDirectory(repository)
		if err != nil {
			return onboarding.State{}, err
		}
	}
	state, err := onboarding.Load(filepath.Join(directory, "state.json"))
	if err != nil {
		return onboarding.State{}, fmt.Errorf("Watchtower is not initialized; run `watchtower init`: %w", err)
	}
	return state, nil
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `AO Watchtower — supervised CI repair for AO

Usage:
  watchtower up [--auto-fix]        Set up if needed, then monitor
  watchtower init [--repo PATH]     Perform one-time local setup
  watchtower status [--repo PATH]   Show local status
  watchtower stats [--repo PATH]    Show durable repair outcomes
  watchtower serve -config FILE     Run webhook/server configuration
  watchtower demo                   Run the fake-boundary UI demo
  watchtower version                Print the build version`)
}

func demoConfig() config.Config {
	c := config.Defaults()
	c.Demo = true
	c.Listen = "127.0.0.1:8787"
	c.SQLitePath = filepath.Join(os.TempDir(), fmt.Sprintf("watchtower-demo-%d.db", time.Now().UnixNano()))
	r, _ := domain.ParseRepository("demo/repo")
	c.RepositoryProjects = []domain.RepositoryProject{{Repository: r, AOProjectID: "demo-project"}}
	c.WebhookSecret = "demo-webhook-secret"
	c.AdminToken = "demo-admin-token"
	c.CallbackSecret = "demo-callback-secret"
	c.CallbackBaseURL = "http://" + c.Listen
	return c
}

type readyHook func(context.Context, http.Handler, string, *ledger.Ledger, *service.Lifecycle) error

// runtimeOptions carries the operator limits that the lifecycle enforces. They
// are separate from config.Config because they are process flags rather than
// durable installation state.
type runtimeOptions struct {
	MaxConcurrentInvestigations int
}

func serve(ctx context.Context, c config.Config, options runtimeOptions, ready readyHook) error {
	if err := c.ValidateRuntime(); err != nil {
		return err
	}
	l, e := ledger.Open(c.SQLitePath)
	if e != nil {
		return e
	}
	defer l.Close()
	var client service.AOClient
	if c.Demo {
		client = demoAO{}
	} else {
		runner, x := ao.NewRunner(c.AOExecutable, c.AOTimeout, c.AOOutputLimit, nil)
		if x != nil {
			return x
		}
		client, x = ao.NewClient(runner)
		if x != nil {
			return x
		}
	}
	life, e := service.NewLifecycle(l, client, service.Options{
		CallbackBaseURL:             c.CallbackBaseURL,
		CallbackSecret:              []byte(c.CallbackSecret),
		MaxConcurrentInvestigations: options.MaxConcurrentInvestigations,
	})
	if e != nil {
		return e
	}
	hook, e := intake.NewHandler([]byte(c.WebhookSecret), c, l, life)
	if e != nil {
		return e
	}
	ui, e := web.New(l, life, c.AdminToken, c.Demo)
	if e != nil {
		return e
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", hook)
	mux.Handle("/", ui.Handler())
	srv := &http.Server{Addr: c.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	listener, e := net.Listen("tcp", c.Listen)
	if e != nil {
		return e
	}
	defer listener.Close()
	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("Watchtower listening on http://%s%s", listener.Addr(), map[bool]string{true: " (DEMO MODE)", false: ""}[c.Demo])
		if e := srv.Serve(listener); e != nil && e != http.ErrServerClosed {
			serverErrors <- e
		}
	}()
	if ready != nil {
		if err := ready(ctx, hook, listener.Addr().String(), l, life); err != nil {
			_ = srv.Close()
			return err
		}
	}
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		return fmt.Errorf("server: %w", err)
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := srv.Shutdown(shutdownContext); e != nil {
		return fmt.Errorf("shutdown: %w", e)
	}
	return nil
}
func newDemoRequest(url, secret string) *http.Request {
	body := []byte(`{"action":"completed","repository":{"name":"repo","owner":{"login":"demo"}},"check_suite":{"id":1,"conclusion":"failure","head_sha":"abcdef0123456789","pull_requests":[{"number":1,"head":{"sha":"abcdef0123456789"}}]}}`)
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write(body)
	r, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", "check_suite")
	r.Header.Set("X-GitHub-Delivery", fmt.Sprintf("demo-%d", time.Now().UnixNano()))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	return r
}
func demoDelivery(url, secret string) {
	r := newDemoRequest(url, secret)
	if resp, e := http.DefaultClient.Do(r); e != nil {
		log.Printf("demo intake: %v", e)
	} else {
		resp.Body.Close()
		log.Printf("demo intake status: %s", resp.Status)
	}
}
