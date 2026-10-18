package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csidriver "pve-csi.local/csi-plugin/internal/csi"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestStorageErrorsReachCSI(t *testing.T) {
	const secret = "fake-secret-must-not-leak"
	const missing = `{"errors":{"storage":"No such storage."},"message":"` + secret + `","data":null}`
	const path = "/api2/json/nodes/pve/storage/missing-pool/status"
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		missing bool
		kind    volume.FailureKind
		code    codes.Code
	}{
		{"missing storage", 400, missing, true, volume.FailureNotFound, codes.FailedPrecondition},
		{"other validation", 400, `{"errors":{"storage":"invalid value"}}`, false, volume.FailureInternal, codes.Internal},
		{"message only", 400, `{"message":"No such storage."}`, false, volume.FailureInternal, codes.Internal},
		{"multiple invalid fields", 400, `{"errors":{"storage":"No such storage.","node":"invalid"}}`, false, volume.FailureInternal, codes.Internal},
		{"malformed JSON", 400, `{"errors":`, false, volume.FailureInternal, codes.Internal},
		{"invalid field type", 400, `{"errors":{"storage":true}}`, false, volume.FailureInternal, codes.Internal},
		{"body at limit", 400, missing + strings.Repeat(" ", (64<<10)-len(missing)), true, volume.FailureNotFound, codes.FailedPrecondition},
		{"oversized body", 400, missing + strings.Repeat(" ", (64<<10)+1-len(missing)), false, volume.FailureInternal, codes.Internal},
		{"unauthenticated", 401, missing, false, volume.FailureUnauthenticated, codes.Unauthenticated},
		{"forbidden", 403, missing, false, volume.FailurePermissionDenied, codes.PermissionDenied},
		{"HTTP not found", 404, missing, false, volume.FailureNotFound, codes.FailedPrecondition},
		{"rate limited", 429, missing, false, volume.FailureUnavailable, codes.Unavailable},
		{"server failure", 500, missing, false, volume.FailureUnavailable, codes.Unavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/api2/json/access/permissions" && r.URL.Query().Get("path") == "/storage/missing-pool" {
					io.WriteString(w, `{"data":{"/storage/missing-pool":{"Datastore.Audit":1,"Datastore.Allocate":1,"Datastore.AllocateSpace":1}}}`)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != path {
					t.Errorf("unexpected PVE request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)
			client := &Client{httpClient: server.Client(), config: Config{Endpoint: server.URL, TokenID: "test", TokenSecret: secret}}
			_, apiErr := client.Storage(context.Background(), "pve", "missing-pool")
			if apiErr == nil {
				t.Fatal("expected a PVE error")
			}
			// Both the semantic error and HTTP facts must survive adapter wrapping.
			mapped := mapError(fmt.Errorf("storage query: %w", apiErr))
			if got := errors.Is(mapped, ErrStorageNotFound); got != tt.missing {
				t.Errorf("ErrStorageNotFound = %v, want %v", got, tt.missing)
			}
			var httpErr *HTTPError
			if !errors.As(mapped, &httpErr) || httpErr.Method != http.MethodGet || httpErr.Path != path || httpErr.StatusCode != tt.status {
				t.Fatalf("HTTP error facts lost: %v", httpErr)
			}
			var backendErr *volume.BackendError
			if !errors.As(mapped, &backendErr) || backendErr.Kind != tt.kind {
				t.Fatalf("backend error = %+v, want kind %v", backendErr, tt.kind)
			}
			gateway, err := NewStorageGateway(client)
			if err != nil {
				t.Fatal(err)
			}
			service, err := volume.NewService(gateway, domain.Scope{Node: "pve", OwnerVMID: 7777})
			if err != nil {
				t.Fatal(err)
			}
			controller, err := csidriver.NewController(csidriver.Config{DriverName: "pve-csi.local"}, service)
			if err != nil {
				t.Fatal(err)
			}
			response, csiErr := controller.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name: "error-regression", Parameters: map[string]string{"storage": "missing-pool"},
				VolumeCapabilities: []*csi.VolumeCapability{{
					AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
					AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
				}},
			})
			if response != nil || status.Code(csiErr) != tt.code {
				t.Fatalf("CreateVolume = %v, %v; want nil, %v", response, csiErr, tt.code)
			}
			message := status.Convert(csiErr).Message()
			if !strings.HasPrefix(message, "[check_storage_status] pve/missing-pool: ") {
				t.Errorf("missing failure step: %s", message)
			}
			if tt.missing && !strings.Contains(message, "PVE storage was not found; check the storage parameter in StorageClass") {
				t.Errorf("missing actionable storage hint: %s", message)
			}
			for _, text := range []string{apiErr.Error(), httpErr.Error(), mapped.Error(), backendErr.Hint, csiErr.Error()} {
				if strings.Contains(text, secret) {
					t.Error("response text leaked into an error")
				}
			}
		})
	}
}

func TestUnreadableErrorBodyPreservesHTTPError(t *testing.T) {
	httpErr := &HTTPError{Method: http.MethodGet, Path: "/storage/status", StatusCode: http.StatusBadRequest}
	// Even valid-looking JSON is not trustworthy if reading the body failed.
	body := io.MultiReader(strings.NewReader(`{"errors":{"storage":"No such storage."}}`), iotest.ErrReader(errors.New("read failed")))
	if err := decodeAPIError(httpErr, body); err != httpErr {
		t.Fatalf("got %v, want original HTTP error", err)
	}
}
