// Package polling provides the zero-tunnel local GitHub intake mode. It reads
// narrow PR/check facts through the authenticated gh CLI, then submits them to
// Watchtower's existing signed and idempotent webhook boundary.
package polling

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

const outputLimit = 4 << 20

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 2048 {
			message = message[:2048]
		}
		return nil, fmt.Errorf("run %s: %s", executable, message)
	}
	if stdout.Len() > outputLimit {
		return nil, fmt.Errorf("run %s: output exceeds limit", executable)
	}
	return stdout.Bytes(), nil
}

type Client struct {
	executable string
	runner     Runner
}

type Observation struct {
	PullNumber int64
	HeadSHA    string
	Conclusion string
	DetailsURL string
}

func NewClient(executable string, runner Runner) (*Client, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(executable) != executable {
		return nil, fmt.Errorf("GitHub CLI executable is required")
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Client{executable: executable, runner: runner}, nil
}

func (c *Client) ListCompleted(ctx context.Context, repository domain.Repository) ([]Observation, error) {
	if err := repository.Validate(); err != nil {
		return nil, err
	}
	payload, err := c.runner.Run(ctx, c.executable, "pr", "list", "--repo", repository.String(), "--state", "open", "--limit", "100", "--json", "number,headRefOid,statusCheckRollup,url")
	if err != nil {
		return nil, err
	}
	var pulls []struct {
		Number            int64  `json:"number"`
		HeadRefOID        string `json:"headRefOid"`
		URL               string `json:"url"`
		StatusCheckRollup []struct {
			Type       string `json:"__typename"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
			DetailsURL string `json:"detailsUrl"`
			TargetURL  string `json:"targetUrl"`
		} `json:"statusCheckRollup"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&pulls); err != nil {
		return nil, fmt.Errorf("decode GitHub PR checks: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	observations := make([]Observation, 0, len(pulls))
	for _, pull := range pulls {
		if pull.Number <= 0 || len(pull.StatusCheckRollup) == 0 {
			continue
		}
		conclusion, detailsURL, complete := aggregate(pull.StatusCheckRollup, pull.URL)
		if !complete {
			continue
		}
		facts := domain.CheckSuiteFacts{ProviderID: 1, Repository: repository, PullNumber: pull.Number, HeadSHA: strings.ToLower(pull.HeadRefOID), Conclusion: conclusion}
		if err := facts.Validate(); err != nil {
			return nil, fmt.Errorf("invalid GitHub PR %d check data: %w", pull.Number, err)
		}
		observations = append(observations, Observation{PullNumber: pull.Number, HeadSHA: facts.HeadSHA, Conclusion: conclusion, DetailsURL: detailsURL})
	}
	return observations, nil
}

func aggregate(checks []struct {
	Type       string `json:"__typename"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
	DetailsURL string `json:"detailsUrl"`
	TargetURL  string `json:"targetUrl"`
}, fallback string) (string, string, bool) {
	conclusion := "success"
	details := fallback
	for _, check := range checks {
		state := strings.ToUpper(check.State)
		status := strings.ToUpper(check.Status)
		result := strings.ToUpper(check.Conclusion)
		if check.Type == "StatusContext" {
			if state == "PENDING" || state == "EXPECTED" || state == "" {
				return "", "", false
			}
			if state == "FAILURE" || state == "ERROR" {
				conclusion = "failure"
				if check.TargetURL != "" {
					details = check.TargetURL
				}
			}
			continue
		}
		if status != "COMPLETED" {
			return "", "", false
		}
		if result == "FAILURE" {
			conclusion = "failure"
			if check.DetailsURL != "" {
				details = check.DetailsURL
			}
		}
	}
	return conclusion, details, true
}

type Poller struct {
	client     *Client
	repository domain.Repository
	secret     []byte
	handler    http.Handler
	interval   time.Duration
	logger     *log.Logger
}

func New(client *Client, repository domain.Repository, secret []byte, handler http.Handler, interval time.Duration, logger *log.Logger) (*Poller, error) {
	if client == nil || handler == nil || len(secret) == 0 {
		return nil, fmt.Errorf("polling client, signed handler, and secret are required")
	}
	if err := repository.Validate(); err != nil {
		return nil, err
	}
	if interval <= 0 {
		return nil, fmt.Errorf("poll interval must be positive")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Poller{client: client, repository: repository, secret: append([]byte(nil), secret...), handler: handler, interval: interval, logger: logger}, nil
}

func (p *Poller) Run(ctx context.Context) {
	p.poll(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) PollOnce(ctx context.Context) error {
	observations, err := p.client.ListCompleted(ctx, p.repository)
	if err != nil {
		return err
	}
	for _, observation := range observations {
		if err := p.deliver(ctx, observation); err != nil {
			return err
		}
	}
	return nil
}

func (p *Poller) poll(ctx context.Context) {
	if err := p.PollOnce(ctx); err != nil && ctx.Err() == nil {
		p.logger.Printf("GitHub poll: %v", err)
	}
}

func (p *Poller) deliver(ctx context.Context, observation Observation) error {
	providerDigest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", p.repository, observation.PullNumber, observation.HeadSHA)))
	providerID := int64(binary.BigEndian.Uint64(providerDigest[:8]) & uint64(^uint64(0)>>1))
	if providerID == 0 {
		providerID = 1
	}
	event := struct {
		Action     string `json:"action"`
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		CheckSuite struct {
			ID         int64  `json:"id"`
			Conclusion string `json:"conclusion"`
			HeadSHA    string `json:"head_sha"`
			DetailsURL string `json:"details_url,omitempty"`
			Pulls      []struct {
				Number int64 `json:"number"`
				Head   struct {
					SHA string `json:"sha"`
				} `json:"head"`
			} `json:"pull_requests"`
		} `json:"check_suite"`
	}{Action: "completed"}
	event.Repository.Name = p.repository.Name
	event.Repository.Owner.Login = p.repository.Owner
	event.CheckSuite.ID = providerID
	event.CheckSuite.Conclusion = observation.Conclusion
	event.CheckSuite.HeadSHA = observation.HeadSHA
	event.CheckSuite.DetailsURL = observation.DetailsURL
	event.CheckSuite.Pulls = make([]struct {
		Number int64 `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}, 1)
	event.CheckSuite.Pulls[0].Number = observation.PullNumber
	event.CheckSuite.Pulls[0].Head.SHA = observation.HeadSHA
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write(body)
	deliveryDigest := sha256.Sum256(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://watchtower.local/webhooks/github", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("X-GitHub-Event", "check_suite")
	request.Header.Set("X-GitHub-Delivery", "poll-"+hex.EncodeToString(deliveryDigest[:]))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response := &responseCapture{header: make(http.Header)}
	p.handler.ServeHTTP(response, request)
	if response.status != http.StatusAccepted {
		return fmt.Errorf("signed local intake returned HTTP %d", response.status)
	}
	return nil
}

type responseCapture struct {
	header http.Header
	status int
}

func (r *responseCapture) Header() http.Header    { return r.header }
func (r *responseCapture) WriteHeader(status int) { r.status = status }
func (r *responseCapture) Write(payload []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return io.Discard.Write(payload)
}

func ensureEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode GitHub PR checks: multiple JSON values")
	}
	return nil
}
