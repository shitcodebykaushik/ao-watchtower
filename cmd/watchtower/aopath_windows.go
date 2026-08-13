//go:build windows

package main

import (
	"os"
	"path/filepath"
)

const aoExecutableName = "ao.exe"

// aoInstallCandidates lists the Agent Orchestrator desktop install locations
// checked when ao.exe is not on PATH. Electron desktop builds install per user
// under LOCALAPPDATA and machine-wide under Program Files.
func aoInstallCandidates() []string {
	var candidates []string
	relative := filepath.Join("Agent Orchestrator", "resources", "daemon", "ao.exe")
	for _, variable := range []string{"LOCALAPPDATA", "PROGRAMFILES", "PROGRAMFILES(X86)"} {
		base, ok := os.LookupEnv(variable)
		if !ok || base == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(base, relative))
		if variable == "LOCALAPPDATA" {
			candidates = append(candidates, filepath.Join(base, "Programs", relative))
		}
	}
	return candidates
}

// isExecutableMode reports whether a candidate is runnable. Windows has no
// execute bit, so reaching a regular .exe file is the available signal.
func isExecutableMode(os.FileInfo) bool { return true }
