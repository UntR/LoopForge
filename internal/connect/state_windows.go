package connect

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func secureStateDirectory(directory string) error {
	if directory == "" {
		return errors.New("a dedicated state directory is required")
	}
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("cannot create state directory; its parent must exist")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must be a real dedicated directory")
	}
	// Never rewrite ACLs on an arbitrary user directory containing other files.
	entries, err := os.ReadDir(directory)
	if err != nil {
		return errors.New("cannot inspect state directory")
	}
	for _, entry := range entries {
		if entry.Name() != "state.json" && entry.Name() != ".lock" && !strings.HasPrefix(entry.Name(), ".state-") {
			return errors.New("state directory contains unrelated files")
		}
	}
	return privateStateACL(directory, true)
}
func privateStateACL(path string, inherit bool) error {
	current, err := user.Current()
	if err != nil || !strings.HasPrefix(current.Uid, "S-1-") {
		return errors.New("cannot determine Windows credential owner")
	}
	flags := ""
	if inherit {
		flags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;" + current.Uid + ")")
	if err != nil {
		return errors.New("cannot create private state ACL")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return errors.New("cannot read private state ACL")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return errors.New("cannot restrict connector state ACL")
	}
	return nil
}
func checkStateFile(path string, info os.FileInfo) error { return privateStateACL(path, false) }

func lockState(directory string) (func(), error) {
	path := filepath.Join(directory, ".lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("invalid connector lock file")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("cannot open connector state lock")
	}
	overlap := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap); err != nil {
		file.Close()
		return nil, errors.New("another connector is using this state directory")
	}
	return func() { _ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlap); _ = file.Close() }, nil
}
