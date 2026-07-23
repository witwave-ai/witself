//go:build !windows

package transcriptcapture

import "os"

func replaceFileAtomic(source, destination string) error {
	return os.Rename(source, destination)
}

func replacementCommitIdentityMatches(staged, _, committed os.FileInfo) bool {
	return os.SameFile(staged, committed)
}
