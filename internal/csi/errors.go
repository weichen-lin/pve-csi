package csidriver

import (
	"context"
	"errors"

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
		return status.Error(codes.InvalidArgument, "invalid v1 volume ID")
	case errors.Is(err, volume.ErrNotFound):
		return status.Error(codes.NotFound, "volume was not found in the configured scope")
	case errors.Is(err, domain.ErrConflict):
		return status.Error(codes.FailedPrecondition, "volume metadata does not match the expected identity or capacity")
	default:
		return csiBackendError(err)
	}
}

func csiBackendError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "volume request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "volume request deadline exceeded; retry with the same name and configuration")
	case errors.Is(err, volume.ErrBusy):
		return status.Error(codes.Aborted, "a CreateVolume request for this name is already in progress; retry")
	case errors.Is(err, volume.ErrUnconfirmed):
		return status.Error(codes.Unavailable, "allocation outcome is unconfirmed; retry with the same name and configuration")
	case errors.Is(err, domain.ErrConflict):
		return status.Error(codes.AlreadyExists, "an incompatible volume already exists for this name")
	case errors.Is(err, domain.ErrCapacity):
		return status.Error(codes.ResourceExhausted, "storage has insufficient available capacity")
	case errors.Is(err, domain.ErrPoolDisabled):
		return status.Error(codes.FailedPrecondition, "requested storage pool is disabled")
	case errors.Is(err, domain.ErrPoolInactive):
		return status.Error(codes.FailedPrecondition, "requested storage pool is inactive")
	case errors.Is(err, domain.ErrStorage):
		return status.Error(codes.FailedPrecondition, "storage does not support the required allocation format")
	}
	var backendErr *volume.BackendError
	if errors.As(err, &backendErr) {
		switch backendErr.Kind {
		case volume.FailureUnauthenticated:
			return status.Error(codes.Unauthenticated, "PVE authentication failed")
		case volume.FailurePermissionDenied:
			return status.Error(codes.PermissionDenied, "PVE denied the storage operation")
		case volume.FailureNotFound:
			return status.Error(codes.FailedPrecondition, "requested PVE storage was not found")
		case volume.FailureUnavailable:
			return status.Error(codes.Unavailable, "PVE service is temporarily unavailable")
		}
	}
	// Backend error messages may carry transport details or untrusted data.
	return status.Error(codes.Internal, "volume backend operation failed")
}
