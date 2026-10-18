package csidriver

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

// Read errors differ from CreateVolume conflicts: an inconsistent observation
// cannot be used, but does not mean the caller requested a duplicate volume.
func csiReadError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return csiBackendError(err)
	}
	switch {
	case errors.Is(err, domain.ErrInvalidHandle):
		return stepStatus(err, codes.InvalidArgument, "invalid v1 volume ID")
	case errors.Is(err, volume.ErrNotFound):
		return stepStatus(err, codes.NotFound, "volume was not found in the configured scope")
	case errors.Is(err, domain.ErrConflict):
		return stepStatus(err, codes.FailedPrecondition, "volume metadata does not match the expected identity or capacity")
	default:
		return csiBackendError(err)
	}
}

func csiBackendError(err error) error {
	if errors.Is(err, volume.ErrUnconfirmed) {
		message := "allocation outcome is unknown; disk may already exist; retry with the same name, node, storage and owner"
		var backendErr *volume.BackendError
		if errors.As(err, &backendErr) {
			_, detail := backendFailure(backendErr)
			message += "; " + detail
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			message += "; request was canceled or timed out before confirmation"
		}
		return stepStatus(err, codes.Unavailable, message)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return stepStatus(err, codes.Canceled, "volume request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return stepStatus(err, codes.DeadlineExceeded, "volume request deadline exceeded; retry with the same name and configuration")
	case errors.Is(err, volume.ErrBusy):
		return stepStatus(err, codes.Aborted, "another volume operation is in progress; wait and retry")
	case errors.Is(err, volume.ErrOutsideScope):
		return stepStatus(err, codes.FailedPrecondition, "volume is outside the configured PVE_NODE or PVE_OWNER_VMID; operation refused")
	case errors.Is(err, domain.ErrConflict):
		return stepStatus(err, codes.AlreadyExists, "an incompatible volume already exists for this name")
	case errors.Is(err, domain.ErrCapacity):
		return stepStatus(err, codes.ResourceExhausted, "storage has insufficient available capacity")
	case errors.Is(err, domain.ErrPoolDisabled):
		return stepStatus(err, codes.FailedPrecondition, "requested storage pool is disabled")
	case errors.Is(err, domain.ErrPoolInactive):
		return stepStatus(err, codes.FailedPrecondition, "requested storage pool is inactive")
	case errors.Is(err, domain.ErrStorage):
		return stepStatus(err, codes.FailedPrecondition, "storage does not support the required allocation format")
	}
	var backendErr *volume.BackendError
	if errors.As(err, &backendErr) {
		code, message := backendFailure(backendErr)
		return stepStatus(err, code, message)
	}
	// Backend error messages may carry transport details or untrusted data.
	return stepStatus(err, codes.Internal, "volume backend operation failed")
}

func csiDeleteError(err error) error {
	switch {
	case errors.Is(err, volume.ErrDeleteUnconfirmed):
		message := "deletion outcome is unknown; PVE may still be deleting the disk; retry DeleteVolume with the same volume_id"
		var backendErr *volume.BackendError
		if errors.As(err, &backendErr) {
			_, detail := backendFailure(backendErr)
			message += "; " + detail
		}
		return stepStatus(err, codes.Unavailable, message)
	case errors.Is(err, domain.ErrInvalidHandle):
		return stepStatus(err, codes.InvalidArgument, "invalid v1 volume ID; deletion refused")
	case errors.Is(err, domain.ErrConflict):
		return stepStatus(err, codes.FailedPrecondition, "disk metadata does not match the requested volume; deletion refused")
	case errors.Is(err, volume.ErrOwnerInUse):
		return stepStatus(err, codes.FailedPrecondition, "owner VMID belongs to a VM or container; this driver only deletes disks under a reserved unused owner")
	case errors.Is(err, volume.ErrDeleteFailed):
		return stepStatus(err, codes.FailedPrecondition, "PVE deletion task failed; inspect Tasks in PVE before retrying")
	default:
		return csiBackendError(err)
	}
}

func backendFailure(err *volume.BackendError) (codes.Code, string) {
	code, message := codes.Internal, "PVE returned an invalid or unexpected response"
	switch err.Kind {
	case volume.FailureUnauthenticated:
		code, message = codes.Unauthenticated, "PVE authentication failed; check PVE_TOKEN_ID and PVE_TOKEN_SECRET"
	case volume.FailurePermissionDenied:
		code, message = codes.PermissionDenied, "PVE denied the operation; check both user and token permissions"
	case volume.FailureNotFound:
		code, message = codes.FailedPrecondition, "PVE resource was not found; check the node and storage names"
	case volume.FailureUnavailable:
		code, message = codes.Unavailable, "PVE is unavailable; check network connectivity and PVE service status"
	case volume.FailurePrecondition:
		code, message = codes.FailedPrecondition, "PVE connection configuration is invalid"
	}
	if err.Hint != "" {
		message = err.Hint
	}
	return code, message
}

func stepStatus(err error, code codes.Code, message string) error {
	var step *volume.StepError
	if errors.As(err, &step) {
		message = fmt.Sprintf("[%s] %s: %s", step.Step, step.Target, message)
	}
	return status.Error(code, message)
}
