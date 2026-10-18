package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// defaultID is the distroless "nonroot" user the image ran as before PUID and
// PGID existed, so a volume created by an older image keeps working.
const defaultID = 65532

// runAs reads PUID and PGID. Both default to defaultID.
func runAs() (uid, gid int, err error) {
	parse := func(key string) (int, error) {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return defaultID, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1<<31-1 {
			return 0, fmt.Errorf("%s must be a numeric id such as 1026, got %q", key, v)
		}
		return n, nil
	}
	if uid, err = parse("PUID"); err != nil {
		return 0, 0, err
	}
	if gid, err = parse("PGID"); err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// chownTree gives everything under dir to uid:gid, skipping what already
// matches so a restart does not rewrite every inode. Symlinks are changed
// themselves, never followed, so a link cannot redirect it outside dir.
func chownTree(dir string, uid, gid int) (changed int, err error) {
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && int(st.Gid) == gid {
			return nil
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return err
		}
		changed++
		return nil
	})
	return changed, err
}
