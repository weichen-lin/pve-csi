package application

import "errors"

var (
	ErrCapacity    = errors.New("requested volume exceeds storage available capacity")
	ErrStorage     = errors.New("unsupported volume storage")
	ErrUnconfirmed = errors.New("volume allocation outcome unconfirmed")
	ErrBusy        = errors.New("a CreateVolume request for this name is already in progress; retry")
)

type FailureKind int

const (
	FailureInternal FailureKind = iota
	FailureUnauthenticated
	FailurePermissionDenied
	FailureNotFound
	FailureUnavailable
)

// BackendError carries classification across the port without exposing an HTTP
// DTO. Context cancellation remains discoverable through Unwrap.
type BackendError struct {
	Kind  FailureKind
	cause error
}

func NewBackendError(kind FailureKind, cause error) *BackendError {
	return &BackendError{Kind: kind, cause: cause}
}

func (e *BackendError) Error() string { return "volume backend operation failed" }
func (e *BackendError) Unwrap() error { return e.cause }
