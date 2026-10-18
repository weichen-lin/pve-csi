package volume

import (
	"errors"

	"pve-csi.local/csi-plugin/internal/domain"
)

// StorageAccess contains effective capabilities for the requested pool, not
// PVE role names or ACLs. Authorization is an application policy.
type StorageAccess struct {
	CanInspect  bool
	CanAllocate bool
}

type PoolState struct {
	Pool   domain.Pool
	Access StorageAccess
}

func (s PoolState) CheckReadable() error {
	if !s.Access.CanInspect {
		return NewBackendError(FailurePermissionDenied, errors.New("storage inspection permission is required"))
	}
	return s.Pool.CheckReady()
}

// CheckAllocatable applies only to a new allocation. Reusing a confirmed
// volume requires neither spare capacity nor allocation permission.
func (s PoolState) CheckAllocatable(requestedBytes int64) error {
	if err := s.CheckReadable(); err != nil {
		return err
	}
	if !s.Access.CanAllocate {
		return NewBackendError(FailurePermissionDenied, errors.New("storage allocation permission is required"))
	}
	return s.Pool.CheckCapacity(requestedBytes)
}

var (
	ErrNotFound    = errors.New("volume not found in the configured scope")
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
