//go:build windows

package main

import "os"

// openPasswordFile refuses on Windows. The rehearsal is driven by a POSIX shell script, and the no-follow,
// non-blocking open that passwordConfig relies on does not exist there; refusing keeps the package building for
// Windows without a weaker open.
func openPasswordFile(string) (*os.File, error) {
	return nil, errPasswordFile
}
