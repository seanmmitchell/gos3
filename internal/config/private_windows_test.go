//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestPrivateFilesWindows: on Windows file modes do not restrict access, so
// secret files would inherit the data directory's ACL (often
// "Authenticated Users: Modify" under C:\). RestrictDir and
// WriteFileAtomic with a private mode set a protected DACL that grants
// only the current user, SYSTEM and Administrators.
func TestPrivateFilesWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".gos3")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RestrictDir(dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, RootCredentialsFileName)
	if err := WriteFileAtomic(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{user.User.Sid.String(): true}
	for _, k := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(k)
		if err != nil {
			t.Fatal(err)
		}
		allowed[sid.String()] = true
	}
	for _, p := range []string{dir, file} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		ctl, _, err := sd.Control()
		if err != nil {
			t.Fatal(err)
		}
		if ctl&windows.SE_DACL_PROTECTED == 0 {
			t.Errorf("%s: DACL not protected (inherits the parent's entries)", p)
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil {
			t.Fatalf("%s: DACL: %v", p, err)
		}
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				t.Fatal(err)
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !allowed[sid.String()] {
				t.Errorf("%s: access granted to %s", p, sid.String())
			}
		}
	}
}
