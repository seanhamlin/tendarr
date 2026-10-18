package main

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// dropPrivileges lets the image be configured with PUID/PGID, for hosts such
// as Synology Container Manager that have no way to set the container user.
//
// Started as root (the image default), it creates the data directory, hands
// it to PUID:PGID, and switches to that user before anything else runs.
// Started as a non-root user (docker run --user, compose `user:`), it leaves
// everything alone: that user was chosen deliberately.
func dropPrivileges(dataDir string, logger *slog.Logger) error {
	uid, gid, err := runAs()
	if err != nil {
		return err
	}
	if os.Getuid() != 0 {
		if os.Getenv("PUID") != "" || os.Getenv("PGID") != "" {
			logger.Warn("PUID/PGID ignored: the container was started as a non-root user", "uid", os.Getuid(), "gid", os.Getgid())
		}
		return nil
	}
	if uid == 0 {
		logger.Warn("running as root because PUID=0; set PUID/PGID to the owner of the data folder instead")
		return nil
	}

	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory %s: %w", dataDir, err)
	}
	changed, err := chownTree(dataDir, uid, gid)
	if err != nil {
		return fmt.Errorf("give %s to %d:%d: %w", dataDir, uid, gid, err)
	}
	if changed > 0 {
		logger.Info("changed data directory ownership", "dir", dataDir, "uid", uid, "gid", gid, "entries", changed)
	}

	// Supplementary groups first, then the group, then the user: once the
	// uid changes there is no permission left to change the others. Go
	// applies these to every thread of the process.
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("set groups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("set gid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("set uid %d: %w", uid, err)
	}
	if os.Getuid() != uid || os.Geteuid() != uid || os.Getgid() != gid {
		return fmt.Errorf("could not switch to %d:%d", uid, gid)
	}
	logger.Info("running as", "uid", uid, "gid", gid)
	return nil
}
