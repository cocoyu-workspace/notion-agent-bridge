//go:build !windows

package proxy

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockCloudJournal(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func syncCloudJournalDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
