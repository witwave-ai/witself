package local

import "errors"

// errLocalLockFileStorage distinguishes the otherwise-impossible failure to
// adopt a successfully opened OS handle from path-safety failures. Callers
// preserve their existing public storage-error classification for this case.
var errLocalLockFileStorage = errors.New("local lock file handle is unavailable")
