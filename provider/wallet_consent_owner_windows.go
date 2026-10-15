//go:build windows

package main

import "os"

// snFileOwnedByCaller has no uid to compare on windows; the mode check does not
// apply there either, so the file is accepted.
func snFileOwnedByCaller(info os.FileInfo) bool { return true }
