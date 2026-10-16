package csidriver

import (
	"context"
	"errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/volume/application"
	"pve-csi.local/csi-plugin/internal/volume/domain"
)

func csiBackendError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "volume request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "volume request deadline exceeded; retry with the same name")
	case errors.Is(err, application.ErrBusy):
		return status.Error(codes.Aborted, "a CreateVolume request for this name is already in progress; retry")
	case errors.Is(err, application.ErrUnconfirmed):
		return status.Error(codes.Unavailable, "allocation outcome is unconfirmed; retry with the same name and configuration")
	case errors.Is(err, domain.ErrConflict):
		return status.Error(codes.AlreadyExists, "an incompatible volume already exists for this name")
	case errors.Is(err, application.ErrCapacity):
		return status.Error(codes.ResourceExhausted, "storage has insufficient available capacity")
	case errors.Is(err, application.ErrStorage):
		return status.Error(codes.FailedPrecondition, "storage does not support the required allocation format")
	}
	var backendErr *application.BackendError
	if errors.As(err, &backendErr) {
		switch backendErr.Kind {
		case application.FailureUnauthenticated:
			return status.Error(codes.Unauthenticated, "PVE authentication failed")
		case application.FailurePermissionDenied:
			return status.Error(codes.PermissionDenied, "PVE denied the storage operation")
		case application.FailureNotFound:
			return status.Error(codes.FailedPrecondition, "configured PVE storage was not found")
		case application.FailureUnavailable:
			return status.Error(codes.Unavailable, "PVE service is temporarily unavailable")
		}
	}
	// Backend error messages may carry transport details or untrusted data.
	return status.Error(codes.Internal, "volume backend operation failed")
}
