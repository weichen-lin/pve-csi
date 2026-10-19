package csidriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestErrorBoundaryPreservesOutcomeAndHidesCause(t *testing.T) {
	const secret = "untrusted-response-with-token"
	denied := volume.NewBackendError(volume.FailurePermissionDenied, "missing required privilege", errors.New(secret))
	for _, tc := range []struct {
		name    string
		err     error
		convert func(error) error
		code    codes.Code
		message string
	}{
		{"permission", denied, csiBackendError, codes.PermissionDenied, "missing required privilege"},
		{"publish conflict", volume.ErrMountConflict, csiReadError, codes.AlreadyExists, "different readonly setting"},
		{"unsafe mount", volume.ErrUnsafeMount, csiReadError, codes.FailedPrecondition, volume.ErrUnsafeMount.Error()},
		{"mount in use", volume.ErrMountInUse, csiReadError, codes.FailedPrecondition, volume.ErrMountInUse.Error()},
		{"volume in use", domain.ErrVolumeInUse, csiDeleteError, codes.FailedPrecondition, domain.ErrVolumeInUse.Error()},
		{"wrong attachment", domain.ErrAttachmentConflict, csiReadError, codes.FailedPrecondition, domain.ErrAttachmentConflict.Error()},
		{"owner in use on publish", domain.ErrOwnerInUse, csiReadError, codes.FailedPrecondition, "reserved disk owner VMID"},
		{"unknown attachment after cancellation", errors.Join(volume.ErrAttachmentUnconfirmed, context.DeadlineExceeded), csiReadError, codes.Unavailable, "outcome is unknown"},
		{"unknown create", errors.Join(volume.ErrUnconfirmed, denied), csiBackendError, codes.Unavailable, "allocation outcome is unknown"},
		{"unknown delete", errors.Join(volume.ErrDeleteUnconfirmed, denied), csiDeleteError, codes.Unavailable, "deletion outcome is unknown"},
		{"canceled", volume.NewBackendError(volume.FailureUnavailable, "", context.Canceled), csiBackendError, codes.Canceled, "canceled"},
		{"raw backend error", errors.New(secret), csiBackendError, codes.Internal, "backend operation failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.convert(volume.AtStep("example_step", "pool", tc.err))
			if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "[example_step] pool") || strings.Contains(err.Error(), secret) {
				t.Fatalf("unexpected client error: %v", err)
			}
		})
	}
}
