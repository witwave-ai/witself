//go:build !windows

package transcriptcapture

import "os"

func replaceFileAtomic(source, destination string) error {
	return os.Rename(source, destination)
}
