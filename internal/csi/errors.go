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
	if errors.Is(err, volume.ErrAttachmentUnconfirmed) {
		return stepStatus(err, codes.Unavailable, "VM disk change outcome is unknown; retry the same request and inspect PVE Tasks")
	}
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
	case errors.Is(err, domain.ErrInvalidHandle):
		return stepStatus(err, codes.InvalidArgument, "invalid v1 volume ID")
	case errors.Is(err, volume.ErrInvalidMountPath):
		return stepStatus(err, codes.InvalidArgument, volume.ErrInvalidMountPath.Error())
	case errors.Is(err, volume.ErrDeviceNotFound):
		return stepStatus(err, codes.Unavailable, volume.ErrDeviceNotFound.Error())
	case errors.Is(err, volume.ErrMountConflict):
		return stepStatus(err, codes.AlreadyExists, volume.ErrMountConflict.Error())
	case errors.Is(err, volume.ErrUnsafeMount):
		return stepStatus(err, codes.FailedPrecondition, volume.ErrUnsafeMount.Error())
	case errors.Is(err, volume.ErrMountInUse):
		return stepStatus(err, codes.FailedPrecondition, volume.ErrMountInUse.Error())
	case errors.Is(err, domain.ErrVolumeInUse):
		return stepStatus(err, codes.FailedPrecondition, domain.ErrVolumeInUse.Error())
	case errors.Is(err, domain.ErrAttachmentConflict):
		return stepStatus(err, codes.FailedPrecondition, domain.ErrAttachmentConflict.Error())
	case errors.Is(err, domain.ErrOwnerInUse):
		return stepStatus(err, codes.FailedPrecondition, domain.ErrOwnerInUse.Error())
	case errors.Is(err, context.Canceled):
		return stepStatus(err, codes.Canceled, "volume request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return stepStatus(err, codes.DeadlineExceeded, "volume request deadline exceeded; retry with the same name and configuration")
	case errors.Is(err, domain.ErrInvalidNodeID):
		return stepStatus(err, codes.InvalidArgument, "node_id must be a nonzero VM UUID")
	case errors.Is(err, domain.ErrNodeNotFound):
		return stepStatus(err, codes.NotFound, "no VM matches the node UUID on the configured PVE_NODE")
	case errors.Is(err, domain.ErrNodeAmbiguous):
		return stepStatus(err, codes.FailedPrecondition, "multiple VMs share the node UUID; configure unique VM UUIDs before retrying")
	case errors.Is(err, domain.ErrInvalidInventory):
		return stepStatus(err, codes.Internal, "PVE VM inventory is invalid or incomplete; cannot confirm node identity")
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
	case errors.Is(err, volume.ErrDeleteFailed):
		return stepStatus(err, codes.FailedPrecondition, "PVE deletion task failed; inspect Tasks in PVE before retrying")
	default:
		return csiBackendError(err)
	}
}

func backendFailure(err *volume.BackendError) (codes.Code, string) {
	code, message := codes.Internal, "volume backend returned an invalid or unexpected response"
	switch err.Kind() {
	case volume.FailureUnauthenticated:
		code, message = codes.Unauthenticated, "volume backend authentication failed"
	case volume.FailurePermissionDenied:
		code, message = codes.PermissionDenied, "volume backend denied the operation"
	case volume.FailureNotFound:
		code, message = codes.FailedPrecondition, "required backend resource was not found"
	case volume.FailureUnavailable:
		code, message = codes.Unavailable, "volume backend is unavailable"
	case volume.FailurePrecondition:
		code, message = codes.FailedPrecondition, "volume backend precondition is not satisfied"
	}
	if err.Hint() != "" {
		message = err.Hint()
	}
	return code, message
}

func stepStatus(err error, code codes.Code, message string) error {
	stepName := ""
	var step *volume.StepError
	if errors.As(err, &step) {
		stepName = step.Step()
		message = fmt.Sprintf("[%s] %s: %s", step.Step(), step.Target(), message)
	}
	return &operationStatus{status: status.New(code, message), step: stepName}
}

// Expose only authored CSI diagnostics; backend errors stay internal.
type operationStatus struct {
	status *status.Status
	step   string
}

func (e *operationStatus) Error() string              { return e.status.Err().Error() }
func (e *operationStatus) GRPCStatus() *status.Status { return e.status }
