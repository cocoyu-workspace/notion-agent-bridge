package proxy

import (
	"golang.org/x/sys/windows"
	"os"
)

func lockCloudJournal(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// Windows does not expose POSIX directory fsync; the file is flushed before rename.
func syncCloudJournalDir(path string) error { return nil }
