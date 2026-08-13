//go:build windows

package onboarding

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// EnforcesUnixPermissions reports whether this platform expresses private state
// protection through POSIX mode bits. Windows does not: the equivalent control
// is an explicit discretionary access control list, applied and verified below.
const EnforcesUnixPermissions = false

// protectFile replaces the inherited ACL of a freshly created state file with a
// protected list that grants full access to the current user only.
func protectFile(file *os.File) error {
	if err := restrictToCurrentUser(file.Name()); err != nil {
		return fmt.Errorf("protect state file: %w", err)
	}
	return nil
}

// protectDirectory creates the private state directory and restricts it the
// same way, so files created later cannot inherit a permissive ACL.
func protectDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := restrictToCurrentUser(path); err != nil {
		return fmt.Errorf("protect state directory: %w", err)
	}
	return nil
}

// verifyPrivate refuses to load state whose ACL grants access to any account
// other than the current user, LocalSystem, or the local Administrators group.
// Those two well-known trustees already have unrestricted machine access, so
// treating them as exposure would reject every ordinary installation.
func verifyPrivate(path string, _ os.FileInfo) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read state file security descriptor: %w", err)
	}
	list, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read state file access control list: %w", err)
	}
	if list == nil {
		// A NULL DACL grants everyone full control.
		return fmt.Errorf("state file %s must not be accessible by other accounts", path)
	}
	allowed, err := privilegedSIDs()
	if err != nil {
		return err
	}
	for index := uint32(0); index < uint32(list.AceCount); index++ {
		var entry *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(list, index, &entry); err != nil {
			return fmt.Errorf("read state file access control entry: %w", err)
		}
		if entry.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		trustee := (*windows.SID)(unsafe.Pointer(uintptr(unsafe.Pointer(entry)) + unsafe.Offsetof(entry.SidStart)))
		if !anyEqual(allowed, trustee) {
			return fmt.Errorf("state file %s must not be accessible by %s", path, trustee.String())
		}
	}
	return nil
}

func restrictToCurrentUser(path string) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	list, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("build access control list: %w", err)
	}
	// PROTECTED_DACL_SECURITY_INFORMATION drops inherited entries so a
	// permissive parent directory cannot widen access to Watchtower secrets.
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, list, nil); err != nil {
		return fmt.Errorf("apply access control list: %w", err)
	}
	return nil
}

func currentUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve current Windows user: %w", err)
	}
	return user.User.Sid, nil
}

// privilegedSIDs returns the trustees whose access to a per-user file is not
// meaningful exposure: the owner itself, LocalSystem, and Administrators.
func privilegedSIDs() ([]*windows.SID, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sids := []*windows.SID{user}
	for _, wellKnown := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(wellKnown)
		if err != nil {
			return nil, fmt.Errorf("resolve well-known Windows account: %w", err)
		}
		sids = append(sids, sid)
	}
	return sids, nil
}

func anyEqual(candidates []*windows.SID, subject *windows.SID) bool {
	for _, candidate := range candidates {
		if windows.EqualSid(candidate, subject) {
			return true
		}
	}
	return false
}
