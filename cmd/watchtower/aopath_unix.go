//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

const aoExecutableName = "ao"

// aoInstallCandidates lists the Agent Orchestrator desktop install locations
// checked when `ao` is not on PATH.
func aoInstallCandidates() []string {
	candidates := []string{
		"/usr/lib/agent-orchestrator/resources/daemon/ao",
		"/Applications/Agent Orchestrator.app/Contents/Resources/daemon/ao",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", "ao"),
			filepath.Join(home, "Applications", "Agent Orchestrator.app", "Contents", "Resources", "daemon", "ao"),
		)
	}
	return candidates
}

// isExecutableMode reports whether a candidate carries a POSIX execute bit.
func isExecutableMode(info os.FileInfo) bool { return info.Mode()&0111 != 0 }
