package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/getpiper/piper/internal/config"
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

// resolveTokenDataDir picks the directory holding the DB `piperd token`
// operates on. Explicit PIPER_DATA_DIR always wins; otherwise, on a
// systemd-managed box, it targets the service's state dir — dropping this
// process to the dir owner's uid/gid when root, and failing with the exact
// sudo command to run when not — so the command can never silently write a
// DB the running service ignores (#134). args is the token subcommand line,
// echoed in that message.
func resolveTokenDataDir(args []string) (string, error) {
	if v := os.Getenv("PIPER_DATA_DIR"); v != "" {
		return v, nil
	}
	if !config.SystemManaged() {
		return config.DefaultDataDir(), nil
	}
	if _, err := os.Stat(config.SystemStateDir); os.IsNotExist(err) {
		return "", fmt.Errorf("service data dir %s does not exist; start the service first: sudo systemctl start piperd", config.SystemStateDir)
	} else if err != nil {
		return "", err
	}
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("this box is systemd-managed and the service data dir %s needs root; run: sudo piperd token %s", config.SystemStateDir, strings.Join(args, " "))
	}
	if err := dropToStateDirOwner(config.SystemStateDir); err != nil {
		return "", err
	}
	return config.SystemStateDir, nil
}
