package iam

// wrappedError is an error with its own message that still matches a base
// sentinel error via errors.Is.
type wrappedError struct {
	msg  string
	base error
}

func (e *wrappedError) Error() string { return e.msg }
func (e *wrappedError) Unwrap() error { return e.base }

// wrapf returns an error that matches base (errors.Is) and reads
// "<base>: <detail>".
func wrapf(base error, detail string) error {
	return &wrappedError{msg: base.Error() + ": " + detail, base: base}
}

// Additional errors. Each one matches one of the contract sentinels with
// errors.Is so callers that only know the contract errors map them correctly.
var (
	// ErrKeyExists is returned when a caller supplied access key is already
	// in use (by any user or the root credentials). It matches ErrUserExists.
	ErrKeyExists error = &wrappedError{msg: "iam: access key already exists", base: ErrUserExists}
	// ErrInvalidKey is returned for malformed caller supplied access/secret
	// keys or key status values. It matches ErrInvalidName.
	ErrInvalidKey error = &wrappedError{msg: "iam: invalid access key, secret key or status", base: ErrInvalidName}
	// ErrLimitExceeded is returned when a mutation would take the IAM state
	// past one of its limits (maxUsers, maxCustomPolicies or
	// maxStateWriteSize). It matches ErrTooManyKeys, the contract error
	// for the per-user key limit, so callers map it the same way (400).
	ErrLimitExceeded error = &wrappedError{msg: "iam: limit exceeded", base: ErrTooManyKeys}
)
