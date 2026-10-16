package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"pve-csi.local/csi-plugin/internal/volume/application"
)

func TestGatewayHTTPClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   application.FailureKind
	}{
		{401, application.FailureUnauthenticated}, {403, application.FailurePermissionDenied},
		{404, application.FailureNotFound}, {429, application.FailureUnavailable},
		{500, application.FailureUnavailable}, {503, application.FailureUnavailable},
		{400, application.FailureInternal}, {201, application.FailureInternal},
	} {
		err := mapError(&pveapi.HTTPError{StatusCode: tc.status})
		var failure *application.BackendError
		if !errors.As(err, &failure) || failure.Kind != tc.kind {
			t.Fatalf("HTTP %d: %v", tc.status, err)
		}
	}
	if !errors.Is(mapError(context.Canceled), context.Canceled) || !errors.Is(mapError(context.DeadlineExceeded), context.DeadlineExceeded) {
		t.Fatal("context cause was lost")
	}
	if strings.Contains(mapError(errors.New("secret-value")).Error(), "secret-value") {
		t.Fatal("adapter exposed raw backend message")
	}
	if mapError(nil) != nil {
		t.Fatal("nil error changed")
	}
}
