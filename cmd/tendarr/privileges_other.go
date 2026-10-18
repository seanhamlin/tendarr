//go:build !linux

package main

import "log/slog"

// dropPrivileges only acts in the Linux container; running the binary
// directly elsewhere (go run on a Mac) keeps the current user.
func dropPrivileges(string, *slog.Logger) error { return nil }
