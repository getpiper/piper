package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/piperbox/piper/internal/config"
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

// stateOwner is the uid/gid that must own the token DB files so the service's
// DynamicUser can reopen them (#134).
type stateOwner struct{ uid, gid int }

// chownDataFiles chowns the token DB and its SQLite side files to uid/gid, so a
// DynamicUser service can reopen the -wal/-shm this command may create. Side
// files absent after a checkpointed op are skipped.
func chownDataFiles(dir string, uid, gid int) error {
	for _, name := range []string{"piper.db", "piper.db-wal", "piper.db-shm"} {
		if err := os.Chown(filepath.Join(dir, name), uid, gid); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
	}
	return nil
}

// resolveTokenDataDir picks the directory holding the DB `piperd token`
// operates on, and (on a systemd box) the owner its DB files must be chowned
// back to. Explicit PIPER_DATA_DIR always wins; otherwise, on a systemd-managed
// box, it targets the service's state dir, failing with the exact sudo command
// to run when not root — so the command can never silently write a DB the
// running service ignores (#134). args is the token subcommand line, echoed in
// that message.
//
// It stays root rather than dropping to the dir owner: the shipped unit runs
// DynamicUser with StateDirectory=, so systemd stores the real state under a
// 0700 root-owned /var/lib/private and symlinks the state dir into it. Only
// root can traverse that wrapper, so a dropped process cannot reach its own
// state dir (#212). We instead operate as root and chown the created DB files
// back to the dir owner afterward (see chownDataFiles), which keeps #134's
// guarantee. A returned owner of nil means no chown is needed.
func resolveTokenDataDir(args []string) (string, *stateOwner, error) {
	if v := os.Getenv("PIPER_DATA_DIR"); v != "" {
		return v, nil, nil
	}
	if !config.SystemManaged() {
		return config.DefaultDataDir(), nil, nil
	}
	if _, err := os.Stat(config.SystemStateDir); os.IsNotExist(err) {
		return "", nil, fmt.Errorf("service data dir %s does not exist; start the service first: sudo systemctl start piperd", config.SystemStateDir)
	} else if err != nil {
		return "", nil, err
	}
	if os.Geteuid() != 0 {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = shellQuote(a)
		}
		return "", nil, fmt.Errorf("this box is systemd-managed and the service data dir %s needs root; run: sudo piperd token %s", config.SystemStateDir, strings.Join(quoted, " "))
	}
	uid, gid, err := ownerOf(config.SystemStateDir)
	if err != nil {
		return "", nil, err
	}
	return config.SystemStateDir, &stateOwner{uid, gid}, nil
}

// shellQuote renders s as a single POSIX-shell word so the sudo hint above can
// be copy-pasted verbatim. A word made only of safe characters is returned
// unchanged; anything else (spaces, quotes, globs, …) is wrapped in single
// quotes, with any embedded single quote escaped the usual POSIX way.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	const safe = "@%+=:,./-_"
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(safe, r) {
			continue
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}
