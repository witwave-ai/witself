package localcell

// fixedCause is reserved for compile-time, source-safe diagnostics. Never
// convert command output, an underlying error, or an operator value to this type.
type fixedCause string

func (e fixedCause) Error() string { return string(e) }
