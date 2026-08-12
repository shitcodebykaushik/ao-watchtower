package intake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"github.com/agent-orchestrator/ao-watchtower/internal/config"
	"github.com/agent-orchestrator/ao-watchtower/internal/ledger"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func signedRequest(t *testing.T, secret, body []byte, id string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", "check_suite")
	r.Header.Set("X-GitHub-Delivery", id)
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	return r
}
func TestHandlerRejectsInvalidSignatureWithoutDurableAction(t *testing.T) {
	l, e := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	h, e := NewHandler([]byte("secret"), config.Defaults(), l)
	if e != nil {
		t.Fatal(e)
	}
	body := []byte(`not json`)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", "check_suite")
	r.Header.Set("X-GitHub-Delivery", "d")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", w.Code)
	}
	n, err := l.Count(context.Background(), "webhook_deliveries")
	if err != nil || n != 0 {
		t.Fatalf("durable rows=%d err=%v", n, err)
	}
}
func TestHandlerRejectsMalformedVerifiedPayloadWithoutDurableAction(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h, err := NewHandler([]byte("secret"), config.Defaults(), l)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"completed"}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, []byte("secret"), body, "bad-payload"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", w.Code)
	}
	n, err := l.Count(context.Background(), "webhook_deliveries")
	if err != nil || n != 0 {
		t.Fatalf("durable rows=%d err=%v", n, err)
	}
}

func TestHandlerRecordsNonFailureAndUnmapped(t *testing.T) {
	l, e := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	h, e := NewHandler([]byte("secret"), config.Defaults(), l)
	if e != nil {
		t.Fatal(e)
	}
	for i, conclusion := range []string{"success", "failure"} {
		body := []byte(`{"action":"completed","repository":{"name":"repo","owner":{"login":"octo"}},"check_suite":{"id":1,"conclusion":"` + conclusion + `","head_sha":"abcdef0123456789","pull_requests":[{"number":2,"head":{"sha":"abcdef0123456789"}}]}}`)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedRequest(t, []byte("secret"), body, string(rune('a'+i))))
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d", w.Code)
		}
	}
	n, err := l.Count(context.Background(), "spawn_reservations")
	if err != nil || n != 0 {
		t.Fatalf("reservations=%d err=%v", n, err)
	}
}
