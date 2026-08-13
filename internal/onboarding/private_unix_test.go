//go:build !windows

package onboarding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsExposedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("err=%v", err)
	}
}
