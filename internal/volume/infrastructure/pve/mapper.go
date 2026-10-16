package pve

import (
	"errors"
	"net/http"

	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"pve-csi.local/csi-plugin/internal/volume/application"
)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	kind := application.FailureInternal
	var httpErr *pveapi.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			kind = application.FailureUnauthenticated
		case http.StatusForbidden:
			kind = application.FailurePermissionDenied
		case http.StatusNotFound:
			kind = application.FailureNotFound
		default:
			if httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500 {
				kind = application.FailureUnavailable
			}
		}
	}
	return application.NewBackendError(kind, err)
}
