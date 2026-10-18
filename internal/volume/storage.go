package volume

import (
	"context"
	"errors"
	"fmt"

	"pve-csi.local/csi-plugin/internal/domain"
)

// StorageGateway is the application's port to storage. A missing observation
// must be distinguished from a lookup failure or an uncertain write.
type StorageGateway interface {
	InspectPool(context.Context, domain.Placement, int) (PoolState, error)
	LookupVolume(context.Context, domain.Identity) (domain.Allocation, bool, error)
	CheckIdle(context.Context, domain.Placement, int) error
	Allocate(context.Context, domain.Request) (string, error)
	CheckDeletion(context.Context, domain.Identity) error
	Delete(context.Context, domain.Identity) error
}

// StorageAccess contains effective capabilities for the requested pool, not
// PVE role names or ACLs. Authorization is an application policy.
type StorageAccess struct {
	CanInspect            bool
	CanAllocate           bool
	AllocationRequirement string
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

// CheckAllocationPermission applies only to a new allocation. Reusing a confirmed
// volume requires neither spare capacity nor allocation permission.
func (s PoolState) CheckAllocationPermission() error {
	if !s.Access.CanAllocate {
		err := NewBackendError(FailurePermissionDenied, nil)
		err.Hint = "storage allocation permission is required"
		if s.Access.AllocationRequirement != "" {
			err.Hint = "missing " + s.Access.AllocationRequirement + "; check both user and token permissions"
		}
		return err
	}
	return nil
}

var (
	ErrOutsideScope      = errors.New("volume is outside the configured node or owner")
	ErrNotFound          = errors.New("volume not found in the configured scope")
	ErrUnconfirmed       = errors.New("volume allocation outcome unconfirmed")
	ErrBusy              = errors.New("another volume operation is in progress; retry")
	ErrDeleteUnconfirmed = errors.New("volume deletion outcome unconfirmed")
	ErrDeleteFailed      = errors.New("PVE deletion task failed")
	ErrOwnerInUse        = errors.New("reserved disk owner VMID is used by a VM or container")
)

type FailureKind int

const (
	FailureInternal FailureKind = iota
	FailureUnauthenticated
	FailurePermissionDenied
	FailureNotFound
	FailureUnavailable
	FailurePrecondition
)

// BackendError carries classification across the port without exposing an HTTP
// DTO. Context cancellation remains discoverable through Unwrap.
type BackendError struct {
	Kind FailureKind
	// Hint is authored by the adapter, never copied from an HTTP response or token.
	Hint  string
	cause error
}

func NewBackendError(kind FailureKind, cause error) *BackendError {
	return &BackendError{Kind: kind, cause: cause}
}

func (e *BackendError) Error() string { return "volume backend operation failed" }
func (e *BackendError) Unwrap() error { return e.cause }

// StepError identifies the failed operation. Target contains only validated
// resource identifiers; underlying backend messages are not sent to CSI clients.
type StepError struct {
	Step   string
	Target string
	cause  error
}

func AtStep(step, target string, err error) error {
	if err == nil {
		return nil
	}
	return &StepError{Step: step, Target: target, cause: err}
}

func (e *StepError) Error() string { return fmt.Sprintf("[%s] %s: %v", e.Step, e.Target, e.cause) }
func (e *StepError) Unwrap() error { return e.cause }
