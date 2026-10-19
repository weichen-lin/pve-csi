package volume

import (
	"errors"
	"fmt"
)

var (
	ErrOutsideScope      = errors.New("volume is outside the configured node or owner")
	ErrNotFound          = errors.New("volume not found in the configured scope")
	ErrUnconfirmed       = errors.New("volume allocation outcome unconfirmed")
	ErrBusy              = errors.New("another volume operation is in progress; retry")
	ErrDeleteUnconfirmed = errors.New("volume deletion outcome unconfirmed")
	ErrDeleteFailed      = errors.New("volume deletion task failed")
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

// BackendError carries immutable classification and an adapter-authored safe
// hint across the port. HTTP facts and cancellation remain available via Unwrap.
type BackendError struct {
	kind FailureKind
	// Hint is authored by the adapter, never copied from an HTTP response or token.
	hint  string
	cause error
}

// NewBackendError accepts only a locally authored hint, never backend response text.
func NewBackendError(kind FailureKind, hint string, cause error) *BackendError {
	return &BackendError{kind: kind, hint: hint, cause: cause}
}

func (e *BackendError) Kind() FailureKind { return e.kind }
func (e *BackendError) Hint() string      { return e.hint }

// rejectedWrite identifies a failure before the backend accepted a write.
// A task-wait failure is not a rejection of the original delete request.
func rejectedWrite(err error) bool {
	if errors.Is(err, ErrDeleteUnconfirmed) || errors.Is(err, ErrUnconfirmed) {
		return false
	}
	var backend *BackendError
	return errors.As(err, &backend) && (backend.Kind() == FailureUnauthenticated ||
		backend.Kind() == FailurePermissionDenied || backend.Kind() == FailureNotFound)
}

func (e *BackendError) Error() string { return "volume backend operation failed" }
func (e *BackendError) Unwrap() error { return e.cause }

// StepError identifies the failed operation. Target contains only validated
// resource identifiers; underlying backend messages are not sent to CSI clients.
type StepError struct {
	step   string
	target string
	cause  error
}

func AtStep(step, target string, err error) error {
	if err == nil {
		return nil
	}
	return &StepError{step: step, target: target, cause: err}
}

func (e *StepError) Error() string { return fmt.Sprintf("[%s] %s: %v", e.step, e.target, e.cause) }
func (e *StepError) Unwrap() error { return e.cause }

func (e *StepError) Step() string   { return e.step }
func (e *StepError) Target() string { return e.target }
