package pve

import (
	"context"
	"encoding/json"
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

func TestStorageReadPermissionCombinations(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		grants                  []string
		withoutOwner, withOwner bool
		ownerReads              int
	}{
		{"none", nil, false, false, 0},
		{"audit", []string{"Datastore.Audit"}, false, true, 1},
		{"space", []string{"Datastore.AllocateSpace"}, false, false, 0},
		{"allocate", []string{"Datastore.Allocate"}, false, false, 0},
		{"audit and space", []string{"Datastore.Audit", "Datastore.AllocateSpace"}, false, true, 1},
		{"audit and allocate", []string{"Datastore.Audit", "Datastore.Allocate"}, true, true, 0},
		{"space and allocate", []string{"Datastore.AllocateSpace", "Datastore.Allocate"}, true, true, 0},
		{"all", []string{"Datastore.Audit", "Datastore.AllocateSpace", "Datastore.Allocate"}, true, true, 0},
	} {
		for _, ownerAllowed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owner=%t", tc.name, ownerAllowed), func(t *testing.T) {
				reads := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" || r.URL.Path != "/api2/json/access/permissions" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						w.WriteHeader(500)
						return
					}
					path := r.URL.Query().Get("path")
					grants := map[string]int{}
					switch path {
					case "/storage/pool":
						for _, grant := range tc.grants {
							grants[grant] = 0
						}
					case "/vms/7777":
						reads++
						if ownerAllowed {
							grants["VM.Config.Disk"] = 0
						}
					default:
						t.Errorf("unexpected permission path: %s", path)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{path: grants}})
				}))
				defer srv.Close()
				g, err := NewStorageGateway(&Client{httpClient: srv.Client(), config: Config{Endpoint: srv.URL}})
				if err != nil {
					t.Fatal(err)
				}
				err = g.checkReadAccess(context.Background(), domain.Placement{Node: "pve", Storage: "pool"}, 7777)
				want := tc.withoutOwner
				if ownerAllowed {
					want = tc.withOwner
				}
				if (err == nil) != want || reads != tc.ownerReads {
					t.Fatalf("access=%v owner reads=%d; want allowed=%t reads=%d", err, reads, want, tc.ownerReads)
				}
				if !want {
					stepName, path := "check_storage_permissions", "/storage/pool"
					if tc.ownerReads != 0 {
						stepName, path = "check_owner_permissions", "/vms/7777"
					}
					var backend *volume.BackendError
					var step *volume.StepError
					if !errors.As(err, &backend) || backend.Kind() != volume.FailurePermissionDenied || !errors.As(err, &step) || step.Step() != stepName || step.Target() != path {
						t.Fatalf("wrong permission failure: %v", err)
					}
				}
			})
		}
	}
}

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
			if !errors.As(mapped, &backendErr) || backendErr.Kind() != tt.kind {
				t.Fatalf("backend error = %+v, want kind %v", backendErr, tt.kind)
			}
			gateway, err := NewStorageGateway(client)
			if err != nil {
				t.Fatal(err)
			}
			attachments, err := NewAttachmentGateway(client)
			if err != nil {
				t.Fatal(err)
			}
			service, err := volume.NewService(gateway, domain.Scope{Node: "pve", OwnerVMID: 7777}, attachments)
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
			for _, text := range []string{apiErr.Error(), httpErr.Error(), mapped.Error(), backendErr.Hint(), csiErr.Error()} {
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

func TestAllocationAuthorizationStaysInPVEAdapter(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new allocation rechecks current permission"
		if existing {
			name = "existing allocation needs no write permission"
		}
		t.Run(name, func(t *testing.T) {
			req, err := domain.NewRequest("retry", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
			if err != nil {
				t.Fatal(err)
			}
			permissionReads, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					t.Errorf("unexpected write: %s", r.Method)
					w.WriteHeader(500)
					return
				}
				switch r.URL.Path {
				case "/api2/json/access/permissions":
					if r.URL.Query().Get("path") == "/nodes/pve" {
						io.WriteString(w, `{"data":{"/nodes/pve":{"Sys.Audit":0}}}`)
						return
					}
					permissionReads++
					// New allocation initially has permission; it is revoked before
					// the application reaches the explicit write permission check.
					if !existing && permissionReads < 3 {
						io.WriteString(w, `{"data":{"/storage/pool":{"Datastore.Audit":1,"Datastore.Allocate":1,"Datastore.AllocateSpace":1}}}`)
					} else {
						io.WriteString(w, `{"data":{"/storage/pool":{"Datastore.Audit":1,"Datastore.Allocate":1}}}`)
					}
				case "/api2/json/nodes/pve/storage/pool/status":
					io.WriteString(w, `{"data":{"type":"lvmthin","content":"images","total":1073741824,"used":0,"avail":1073741824,"enabled":1,"active":1}}`)
				case "/api2/json/nodes/pve/tasks":
					io.WriteString(w, `{"data":[]}`)
				case "/api2/json/cluster/nextid":
					io.WriteString(w, `{"data":"7777"}`)
				case "/api2/json/nodes/pve/storage/pool/content":
					if existing {
						fmt.Fprintf(w, `{"data":[{"volid":%q,"vmid":7777,"format":"raw","content":"images","size":1073741824}]}`, req.AllocationReference())
					} else {
						io.WriteString(w, `{"data":[]}`)
					}
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			t.Cleanup(server.Close)
			client := &Client{httpClient: server.Client(), config: Config{Endpoint: server.URL}}
			gateway, err := NewStorageGateway(client)
			if err != nil {
				t.Fatal(err)
			}
			attachments, err := NewAttachmentGateway(client)
			if err != nil {
				t.Fatal(err)
			}
			service, err := volume.NewService(gateway, domain.Scope{Node: "pve", OwnerVMID: 7777}, attachments)
			if err != nil {
				t.Fatal(err)
			}
			got, err := service.EnsureVolume(context.Background(), req)
			if existing {
				if err != nil || got.ID() != req.Handle() || permissionReads != 1 {
					t.Fatalf("existing volume: %v, %v, permission reads=%d", got, err, permissionReads)
				}
			} else {
				var backend *volume.BackendError
				var step *volume.StepError
				if !errors.As(err, &backend) || backend.Kind() != volume.FailurePermissionDenied || !strings.Contains(backend.Hint(), "Datastore.AllocateSpace on /storage/pool") || !errors.As(err, &step) || step.Step() != "check_allocation_permission" || permissionReads != 3 {
					t.Fatalf("revoked permission: %v, reads=%d", err, permissionReads)
				}
			}
			if writes != 0 {
				t.Fatal("unexpected backend mutation")
			}
		})
	}
}
