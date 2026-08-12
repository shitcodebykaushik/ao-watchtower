package polling

import (
	"context"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/config"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/intake"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
)

type staticRunner struct{ payload []byte }

func (s staticRunner) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	return append([]byte(nil), s.payload...), nil
}

func TestClientListsOnlyCompletedPRChecks(t *testing.T) {
	payload := []byte(`[
  {"number":1,"headRefOid":"abcdef0123456789","url":"https://github.com/acme/app/pull/1","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/acme/app/actions/runs/1"}]},
  {"number":2,"headRefOid":"abcdef0123456790","url":"https://github.com/acme/app/pull/2","statusCheckRollup":[{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""}]},
  {"number":3,"headRefOid":"abcdef0123456791","url":"https://github.com/acme/app/pull/3","statusCheckRollup":[{"__typename":"StatusContext","state":"SUCCESS","targetUrl":"https://ci.example/3"}]}
]`)
	client, err := NewClient("gh", staticRunner{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	repository, _ := domain.ParseRepository("acme/app")
	observations, err := client.ListCompleted(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[0].Conclusion != "failure" || observations[1].Conclusion != "success" {
		t.Fatalf("observations=%#v", observations)
	}
}

type countingProcessor struct{ count int }

func (c *countingProcessor) ProcessReservation(context.Context, ledger.Result, domain.CheckSuiteFacts) error {
	c.count++
	return nil
}

func TestPollerUsesSignedIdempotentIntake(t *testing.T) {
	repository, _ := domain.ParseRepository("acme/app")
	configuration := config.Defaults()
	configuration.RepositoryProjects = []domain.RepositoryProject{{Repository: repository, AOProjectID: "app"}}
	durableLedger, err := ledger.Open(filepath.Join(t.TempDir(), "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer durableLedger.Close()
	processor := &countingProcessor{}
	secret := []byte("polling-test-secret")
	handler, err := intake.NewHandler(secret, configuration, durableLedger, processor)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`[{"number":7,"headRefOid":"abcdef0123456789","url":"https://github.com/acme/app/pull/7","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/acme/app/actions/runs/7"}]}]`)
	client, _ := NewClient("gh", staticRunner{payload: payload})
	poller, err := New(client, repository, secret, handler, time.Second, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if processor.count != 1 {
		t.Fatalf("reservation processing count=%d", processor.count)
	}
	if count, err := durableLedger.Count(context.Background(), "webhook_deliveries"); err != nil || count != 1 {
		t.Fatalf("deliveries=%d err=%v", count, err)
	}
}
