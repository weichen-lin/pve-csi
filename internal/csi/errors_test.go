package csidriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
