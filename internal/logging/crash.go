package logging

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
)

const CrashLogName = "bigo-live-recorder-crash.log"

// CaptureCrashes saves runtime panic/fatal output, including background
// goroutines, without recovering from failures or replacing standard error.
// Call once after acquiring the single-instance lock. Normal starts preserve
// earlier reports; only a nonempty report is rotated into the two archives.
// The runtime owns a duplicate descriptor until process exit.
func CaptureCrashes(dir string) (previous string, err error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, CrashLogName)
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("the crash log path is not a regular file")
		}
		if info.Size() > 0 {
			if err := rotateFiles(path); err != nil {
				return "", err
			}
			previous = path + ".1"
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return previous, err
	}
	if err := f.Chmod(0600); err != nil {
		return previous, errors.Join(err, f.Close())
	}
	err = debug.SetCrashOutput(f, debug.CrashOptions{})
	return previous, errors.Join(err, f.Close())
}
