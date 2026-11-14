package localcell

// The eight checkpoint fixed causes, for errors.Is in the native proof.
var (
	ErrCheckpointStateUnsupportedForTest    = errCheckpointState
	ErrCheckpointIdentityMismatchForTest    = errCheckpointIdentity
	ErrCheckpointSourceUnsupportedForTest   = errCheckpointSource
	ErrCheckpointStorageInsufficientForTest = errCheckpointStorage
	ErrCheckpointLimitReachedForTest        = errCheckpointLimit
	ErrCheckpointIncompleteForTest          = errCheckpointIncomplete
	ErrCheckpointWitnessMismatchForTest     = errCheckpointWitness
	ErrCheckpointCopyFailedForTest          = errCheckpointCopy
)
