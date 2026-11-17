//go:build darwin || linux

package connect

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func secureStateDirectory(directory string) error {
	if directory == "" {
		return errors.New("a dedicated state directory is required")
	}
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("cannot create state directory; its parent must exist")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("state directory must be a private real directory (mode 0700)")
	}
	return nil
}
func checkStateFile(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("state file must not be readable by other users")
	}
	return nil
}
func lockState(directory string) (func(), error) {
	path := filepath.Join(directory, ".lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("invalid connector lock file")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("cannot open connector state lock")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another connector is using this state directory")
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}
