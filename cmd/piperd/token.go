package main

import (
	"fmt"
	"os"
	"syscall"
)

// ownerOf returns the uid/gid owning path.
func ownerOf(path string) (uid, gid int, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s: no owner info", path)
	}
	return int(st.Uid), int(st.Gid), nil
}

// dropToStateDirOwner switches the process to the uid/gid owning dir, so the
// SQLite side files (-wal/-shm) this command creates stay reopenable by the
// service's DynamicUser. A dir already owned by the current euid — including a
// root-owned one before the service's first start, which systemd re-chowns on
// start — is a no-op.
func dropToStateDirOwner(dir string) error {
	uid, gid, err := ownerOf(dir)
	if err != nil {
		return err
	}
	if uid == os.Geteuid() {
		return nil
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	return nil
}
