//go:build !windows

package onboarding

import (
	"fmt"
	"os"
)

// EnforcesUnixPermissions reports whether this platform expresses private state
// protection through POSIX mode bits.
const EnforcesUnixPermissions = true

// protectFile restricts a freshly created state file to its owner.
func protectFile(file *os.File) error {
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("protect state file: %w", err)
	}
	return nil
}

// protectDirectory restricts the private state directory to its owner.
func protectDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	return nil
}

// verifyPrivate refuses to load state that group or other accounts can read.
func verifyPrivate(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("state file %s must not be accessible by group or others", path)
	}
	return nil
}
