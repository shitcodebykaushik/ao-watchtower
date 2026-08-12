package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	secret, body := []byte("secret"), []byte(`{"action":"completed"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if err := VerifySignature(secret, body, valid); err != nil {
		t.Fatal(err)
	}
	for _, signature := range []string{"", "sha1=abc", "sha256=not-hex", "sha256=" + hex.EncodeToString(make([]byte, 32))} {
		if err := VerifySignature(secret, body, signature); err == nil {
			t.Errorf("VerifySignature(%q) succeeded", signature)
		}
	}
}

func TestNormalizeCheckSuiteCompleted(t *testing.T) {
	payload := []byte(`{"action":"completed","repository":{"name":"Repo","owner":{"login":"Octo"}},"check_suite":{"id":12,"conclusion":"failure","head_sha":"ABCDEF0123456789","details_url":"https://github.example/checks/12","url":"https://api.github.example/check-suites/12","pull_requests":[{"number":4,"head":{"sha":"abcdef0123456789"}}]}}`)
	facts, err := NormalizeCheckSuiteCompleted(payload)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Repository.String() != "octo/repo" || facts.PullNumber != 4 || facts.HeadSHA != "abcdef0123456789" {
		t.Fatalf("facts = %#v", facts)
	}
	for _, invalid := range [][]byte{
		[]byte(`{"action":"requested"}`),
		[]byte(`{"action":"completed","repository":{"name":"repo","owner":{"login":"octo"}},"check_suite":{"id":1,"conclusion":"failure","head_sha":"abcdef0","pull_requests":[]}}`),
		[]byte(`{"action":"completed","repository":{"name":"repo","owner":{"login":"octo"}},"check_suite":{"id":1,"conclusion":"failure","head_sha":"abcdef0","pull_requests":[{"number":1,"head":{"sha":"1234567"}},{"number":2,"head":{"sha":"abcdef0"}}]}}`),
	} {
		if _, err := NormalizeCheckSuiteCompleted(invalid); err == nil {
			t.Errorf("NormalizeCheckSuiteCompleted(%s) succeeded", invalid)
		}
	}
}
