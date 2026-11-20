//go:build !windows

package main

import "os"

func isReparsePoint(fi os.FileInfo) bool { return fi.Mode()&os.ModeSymlink != 0 }
func isLocalVolume(string) bool          { return false }

// This helper has no production approval UI on non-Windows hosts.
func confirmFiles(string, []string) bool { return false }
