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
	"github.com/agent-orchestrator/ao-watchtower/internal/ao"
	"github.com/agent-orchestrator/ao-watchtower/internal/config"
	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
	"github.com/agent-orchestrator/ao-watchtower/internal/intake"
	"github.com/agent-orchestrator/ao-watchtower/internal/ledger"
	"github.com/agent-orchestrator/ao-watchtower/internal/service"
	"github.com/agent-orchestrator/ao-watchtower/internal/web"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type demoAO struct{}

func (demoAO) SpawnInvestigatorSession(_ context.Context, r ao.InvestigatorRequest) (ao.Session, error) {
	return ao.Session{ID: "demo-session", ProjectID: r.ProjectID, Status: "running"}, nil
}
func (demoAO) SendApprovedFollowup(context.Context, string, string) (ao.CommandResult, error) {
	return ao.CommandResult{}, nil
}
func main() {
	var file string
	flag.StringVar(&file, "config", os.Getenv("WT_CONFIG"), "JSON configuration file")
	flag.Parse()
	demo := flag.NArg() == 1 && flag.Arg(0) == "demo"
	if flag.NArg() > 0 && !demo {
		log.Fatal("usage: watchtower [-config file] [demo]")
	}
	var c config.Config
	var e error
	if demo {
		c = config.Defaults()
	} else {
		c, e = config.Load(file)
	}
	if demo {
		c = config.Defaults()
		c.Demo = true
		c.Listen = "127.0.0.1:8787"
		c.SQLitePath = filepath.Join(os.TempDir(), fmt.Sprintf("watchtower-demo-%d.db", time.Now().UnixNano()))
		r, _ := domain.ParseRepository("demo/repo")
		c.RepositoryProjects = []domain.RepositoryProject{{Repository: r, AOProjectID: "demo-project"}}
		c.WebhookSecret = "demo-webhook-secret"
		c.AdminToken = "demo-admin-token"
		c.CallbackSecret = "demo-callback-secret"
		c.CallbackBaseURL = "http://" + c.Listen
		e = c.ValidateRuntime()
	}
	if e != nil {
		log.Fatal(e)
	}
	l, e := ledger.Open(c.SQLitePath)
	if e != nil {
		log.Fatal(e)
	}
	defer l.Close()
	var client service.AOClient
	if c.Demo {
		client = demoAO{}
	} else {
		runner, x := ao.NewRunner(c.AOExecutable, c.AOTimeout, c.AOOutputLimit, nil)
		if x != nil {
			log.Fatal(x)
		}
		client, x = ao.NewClient(runner)
		if x != nil {
			log.Fatal(x)
		}
	}
	life, e := service.NewLifecycle(l, client, service.Options{CallbackBaseURL: c.CallbackBaseURL, CallbackSecret: []byte(c.CallbackSecret)})
	if e != nil {
		log.Fatal(e)
	}
	hook, e := intake.NewHandler([]byte(c.WebhookSecret), c, l, life)
	if e != nil {
		log.Fatal(e)
	}
	ui, e := web.New(l, life, c.AdminToken, c.Demo)
	if e != nil {
		log.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", hook)
	mux.Handle("/", ui.Handler())
	srv := &http.Server{Addr: c.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	listener, e := net.Listen("tcp", c.Listen)
	if e != nil {
		log.Fatal(e)
	}
	go func() {
		log.Printf("Watchtower listening on http://%s%s", listener.Addr(), map[bool]string{true: " (DEMO MODE)", false: ""}[c.Demo])
		if e := srv.Serve(listener); e != nil && e != http.ErrServerClosed {
			log.Fatal(e)
		}
	}()
	if demo {
		go demoDelivery("http://"+listener.Addr().String()+"/webhooks/github", c.WebhookSecret)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := srv.Shutdown(ctx); e != nil {
		log.Printf("shutdown: %v", e)
	}
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
