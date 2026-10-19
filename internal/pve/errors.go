package pve

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"pve-csi.local/csi-plugin/internal/volume"
)

// HTTPError preserves status classification without retaining a server body
// or credentials.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("PVE %s %s: HTTP %d (%s)", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
}

var ErrStorageNotFound = errors.New("PVE storage not found")

// decodeAPIError translates recognized PVE errors while preserving HTTP facts.
// Unrecognized or unreadable bodies remain HTTP errors; response text is never retained.
func decodeAPIError(httpErr *HTTPError, response io.Reader) error {
	if httpErr.StatusCode != http.StatusBadRequest {
		return httpErr
	}
	const maxErrorBytes = 64 << 10
	body, err := io.ReadAll(io.LimitReader(response, maxErrorBytes+1))
	if err != nil || len(body) > maxErrorBytes {
		return httpErr
	}
	var validation struct {
		Errors map[string]string `json:"errors"`
	}
	// PVE reports missing storage as a field validation error instead of HTTP 404.
	if json.Unmarshal(body, &validation) == nil && len(validation.Errors) == 1 && validation.Errors["storage"] == "No such storage." {
		return fmt.Errorf("%w: %w", ErrStorageNotFound, httpErr)
	}
	return httpErr
}

func missingPrivilege(path, privilege string) error {
	return volume.NewBackendError(volume.FailurePermissionDenied,
		"missing "+privilege+" on "+path+"; check both user and token permissions", nil)
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var classified *volume.BackendError
	if errors.As(err, &classified) {
		return err
	}
	kind := volume.FailureInternal
	hint := "PVE returned an invalid or unexpected response"
	var httpErr *HTTPError
	if errors.Is(err, ErrStorageNotFound) {
		kind = volume.FailureNotFound
		hint = "PVE storage was not found; check the storage parameter in StorageClass"
	} else if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			kind = volume.FailureUnauthenticated
			hint = "PVE authentication failed; check PVE_TOKEN_ID and PVE_TOKEN_SECRET"
		case http.StatusForbidden:
			kind = volume.FailurePermissionDenied
			hint = "PVE denied the operation; check both user and token permissions"
		case http.StatusNotFound:
			kind = volume.FailureNotFound
			hint = "PVE resource was not found; check the node and storage names"
		default:
			if httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500 {
				kind = volume.FailureUnavailable
				hint = "PVE is unavailable; check network connectivity and PVE service status"
			}
		}
	} else {
		var certificateErr *tls.CertificateVerificationError
		var hostnameErr x509.HostnameError
		var authorityErr x509.UnknownAuthorityError
		var networkErr net.Error
		switch {
		case errors.As(err, &certificateErr), errors.As(err, &hostnameErr), errors.As(err, &authorityErr):
			kind = volume.FailurePrecondition
			hint = "PVE TLS certificate verification failed; check PVE_CA_FILE and the hostname in PVE_ENDPOINT"
		case errors.As(err, &networkErr):
			kind = volume.FailureUnavailable
			hint = "cannot reach PVE; check PVE_ENDPOINT and network connectivity"
		}
	}
	return volume.NewBackendError(kind, hint, err)
}
