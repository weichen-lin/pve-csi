package csidriver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"pve-csi.local/csi-plugin/internal/testutil"
	"pve-csi.local/csi-plugin/internal/volume/application"
	"pve-csi.local/csi-plugin/internal/volume/domain"
	"pve-csi.local/csi-plugin/internal/volume/infrastructure/pve"
)

type testCSIReader func(context.Context, domain.Identity) (domain.Volume, error)

func (f testCSIReader) GetVolume(ctx context.Context, id domain.Identity) (domain.Volume, error) {
	return f(ctx, id)
}

func validationRequest() *csi.ValidateVolumeCapabilitiesRequest {
	return &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testutil.VolumeRequest().Handle(),
		VolumeCapabilities: testCSIRequest().VolumeCapabilities,
	}
}

func validationController(t *testing.T, reader application.VolumeReader) *Controller {
	t.Helper()
	return newTestCSIControllerWithReader(t, testCSIAllocator(func(context.Context, domain.Request) (domain.Volume, error) {
		t.Error("validation called the provisioner")
		return domain.Volume{}, errors.New("unexpected allocation")
	}), reader)
}

func TestCSIValidateConfirmsKnownFieldsWithoutAliasing(t *testing.T) {
	for _, fs := range []string{"", "ext4"} {
		t.Run("filesystem="+fs, func(t *testing.T) {
			var reads int
			controller := validationController(t, testCSIReader(func(_ context.Context, id domain.Identity) (domain.Volume, error) {
				reads++
				if id.ID() != testutil.VolumeRequest().Handle() {
					t.Error("handle changed")
				}
				return testutil.ConfirmedVolume(testutil.VolumeRequest()), nil
			}))
			req := validationRequest()
			req.VolumeCapabilities[0].GetMount().FsType = fs
			req.VolumeCapabilities = append(req.VolumeCapabilities, proto.Clone(req.VolumeCapabilities[0]).(*csi.VolumeCapability))
			req.Parameters = map[string]string{"storage": "test-pool"}
			response, err := controller.ValidateVolumeCapabilities(context.Background(), req)
			if err != nil || response.GetConfirmed() == nil || reads != 1 {
				t.Fatalf("validation = %v, %v", response, err)
			}
			confirmed := response.Confirmed
			if len(confirmed.VolumeCapabilities) != 2 || !proto.Equal(confirmed.VolumeCapabilities[0], req.VolumeCapabilities[0]) || !proto.Equal(confirmed.VolumeCapabilities[1], req.VolumeCapabilities[1]) || confirmed.Parameters["storage"] != "test-pool" {
				t.Fatalf("confirmed fields differ: %v", confirmed)
			}
			req.VolumeCapabilities[0].GetMount().FsType = "xfs"
			req.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
			req.Parameters["storage"] = "changed"
			if confirmed.VolumeCapabilities[0].GetMount().FsType != fs || confirmed.VolumeCapabilities[0].GetAccessMode().Mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER || confirmed.Parameters["storage"] != "test-pool" {
				t.Fatal("response aliases request")
			}
		})
	}
}

func TestCSIValidateRejectsMalformedRequestsBeforeRead(t *testing.T) {
	controller := validationController(t, testCSIReader(func(context.Context, domain.Identity) (domain.Volume, error) {
		t.Error("malformed request reached reader")
		return domain.Volume{}, nil
	}))
	for _, tc := range []struct {
		name   string
		change func(*csi.ValidateVolumeCapabilitiesRequest)
	}{
		{"empty ID", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeId = "" }},
		{"bad ID", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeId = testutil.FakeSecret }},
		{"system disk", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeId = "v1:test-pve:test-pool:vm-482-disk-0" }},
		{"no capabilities", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeCapabilities = nil }},
		{"nil capability", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeCapabilities = []*csi.VolumeCapability{nil} }},
		{"no type", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeCapabilities[0].AccessType = nil }},
		{"nil mount", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Mount{}
		}},
		{"no mode", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeCapabilities[0].AccessMode = nil }},
		{"unknown mode", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_UNKNOWN
		}},
		{"context", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeContext = map[string]string{"key": testutil.FakeSecret}
		}},
		{"secrets", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.Secrets = map[string]string{"token": testutil.FakeSecret}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := validationRequest()
			tc.change(req)
			_, err := controller.ValidateVolumeCapabilities(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument || strings.Contains(err.Error(), testutil.FakeSecret) {
				t.Fatalf("invalid request = %v", err)
			}
		})
	}
	if _, err := controller.ValidateVolumeCapabilities(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil request = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := controller.ValidateVolumeCapabilities(ctx, validationRequest()); status.Code(err) != codes.Canceled {
		t.Fatalf("canceled = %v", err)
	}
}

func TestCSIValidateUnsupportedCapabilities(t *testing.T) {
	unknown := []byte{0xa0, 0x06, 0x01} // Unknown protobuf field 100, varint 1.
	for _, tc := range []struct {
		name   string
		change func(*csi.ValidateVolumeCapabilitiesRequest)
	}{
		{"block", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
		}},
		{"xfs", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeCapabilities[0].GetMount().FsType = "xfs" }},
		{"mount flags", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].GetMount().MountFlags = []string{testutil.FakeSecret}
		}},
		{"mount group", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].GetMount().VolumeMountGroup = testutil.FakeSecret
		}},
		{"multi node", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		}},
		{"new enum", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.VolumeCapabilities[0].AccessMode.Mode = 999 }},
		{"mixed", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			other := proto.Clone(r.VolumeCapabilities[0]).(*csi.VolumeCapability)
			other.GetMount().FsType = "xfs"
			r.VolumeCapabilities = append(r.VolumeCapabilities, other)
		}},
		{"unknown parameter", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.Parameters = map[string]string{"key": testutil.FakeSecret}
		}},
		{"different storage", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.Parameters = map[string]string{"storage": "other-pool"}
		}},
		{"mutable parameter", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.MutableParameters = map[string]string{"key": testutil.FakeSecret}
		}},
		{"unknown request field", func(r *csi.ValidateVolumeCapabilitiesRequest) { r.ProtoReflect().SetUnknown(unknown) }},
		{"unknown capability field", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].ProtoReflect().SetUnknown(unknown)
		}},
		{"unknown mount field", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].GetMount().ProtoReflect().SetUnknown(unknown)
		}},
		{"unknown mode field", func(r *csi.ValidateVolumeCapabilitiesRequest) {
			r.VolumeCapabilities[0].AccessMode.ProtoReflect().SetUnknown(unknown)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			controller := validationController(t, testCSIReader(func(context.Context, domain.Identity) (domain.Volume, error) {
				reads++
				return testutil.ConfirmedVolume(testutil.VolumeRequest()), nil
			}))
			req := validationRequest()
			tc.change(req)
			response, err := controller.ValidateVolumeCapabilities(context.Background(), req)
			if err != nil || response == nil || response.Confirmed != nil || response.Message == "" || reads != 1 {
				t.Fatalf("unsupported = %v, %v; reads=%d", response, err, reads)
			}
			if strings.Contains(response.Message, testutil.FakeSecret) {
				t.Fatal("unsupported response leaked input")
			}
		})
	}
}

func TestCSIValidateReadErrorsAndInconsistentResults(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"missing", application.ErrNotFound, codes.NotFound},
		{"metadata conflict", domain.ErrConflict, codes.FailedPrecondition},
		{"canceled", errors.Join(domain.ErrConflict, context.Canceled), codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"auth", application.NewBackendError(application.FailureUnauthenticated, errors.New(testutil.FakeSecret)), codes.Unauthenticated},
		{"permission", application.NewBackendError(application.FailurePermissionDenied, errors.New(testutil.FakeSecret)), codes.PermissionDenied},
		{"unavailable", application.NewBackendError(application.FailureUnavailable, errors.New(testutil.FakeSecret)), codes.Unavailable},
		{"unknown", errors.New(testutil.FakeSecret), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := validationController(t, testCSIReader(func(context.Context, domain.Identity) (domain.Volume, error) { return domain.Volume{}, tc.err }))
			req := validationRequest()
			req.VolumeCapabilities[0].GetMount().FsType = "xfs"
			_, err := controller.ValidateVolumeCapabilities(context.Background(), req)
			if status.Code(err) != tc.code || strings.Contains(err.Error(), testutil.FakeSecret) {
				t.Fatalf("read error = %v, want %v", err, tc.code)
			}
		})
	}
	for _, volume := range []domain.Volume{{}, testutil.ConfirmedVolume(domain.Request{Name: "other", Node: "test-pve", Storage: "test-pool", OwnerVMID: 482, CapacityBytes: domain.GiB})} {
		controller := validationController(t, testCSIReader(func(context.Context, domain.Identity) (domain.Volume, error) { return volume, nil }))
		if _, err := controller.ValidateVolumeCapabilities(context.Background(), validationRequest()); status.Code(err) != codes.Internal {
			t.Fatalf("inconsistent read = %v", err)
		}
	}
}

func newValidationAPI(t *testing.T) (*testutil.VolumeAPI, csi.ControllerClient) {
	t.Helper()
	req := testutil.VolumeRequest()
	api, client := testutil.NewVolumeAPI(t, req)
	gateway, err := pve.NewStorageGateway(client)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := application.NewReader(gateway, req.Placement(), req.OwnerVMID)
	if err != nil {
		t.Fatal(err)
	}
	return api, csi.NewControllerClient(connectTestCSIController(t, validationController(t, reader)))
}

func TestCSIValidateGRPCReadOnlyAndConcurrent(t *testing.T) {
	api, client := newValidationAPI(t)
	api.Configure(func() {
		api.Entries = []map[string]any{testutil.VolumeEntry(testutil.VolumeRequest())}
		api.StatusResponse = &testutil.VolumeResponse{Status: http.StatusOK, Body: strings.Replace(testutil.VolumeStorageStatus, `"avail":17179869184`, `"avail":0`, 1)}
	})
	ctx := testCSIContext(t)
	for i := 0; i < 2; i++ {
		response, err := client.ValidateVolumeCapabilities(ctx, validationRequest())
		if err != nil || response.GetConfirmed() == nil {
			t.Fatalf("repeat read = %v, %v", response, err)
		}
	}
	const concurrent = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response, err := client.ValidateVolumeCapabilities(ctx, validationRequest())
			if err != nil || response.GetConfirmed() == nil {
				t.Errorf("concurrent read = %v, %v", response, err)
				return
			}
			succeeded.Add(1)
		}()
	}
	close(start)
	wg.Wait()
	if succeeded.Load() != concurrent {
		t.Fatal("some reads failed")
	}
	api.AssertCounts(t, 2+concurrent, 0)
	api.Mu.Lock()
	defer api.Mu.Unlock()
	for _, call := range api.Calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("validation performed a write: %s", call)
		}
	}
}

func TestCSIValidateGRPCBackendFailuresAndScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testutil.VolumeAPI)
		code  codes.Code
	}{
		{"missing", func(*testutil.VolumeAPI) {}, codes.NotFound},
		{"forbidden", func(a *testutil.VolumeAPI) {
			a.ListResponses[1] = testutil.VolumeResponse{Status: 403, Body: testutil.FakeSecret}
		}, codes.PermissionDenied},
		{"failed list", func(a *testutil.VolumeAPI) {
			a.ListResponses[1] = testutil.VolumeResponse{Status: 503, Body: testutil.FakeSecret}
		}, codes.Unavailable},
		{"invalid JSON", func(a *testutil.VolumeAPI) {
			a.ListResponses[1] = testutil.VolumeResponse{Status: 200, Body: testutil.FakeSecret}
		}, codes.Internal},
		{"wrong owner", func(a *testutil.VolumeAPI) {
			entry := testutil.VolumeEntry(a.Req)
			entry["vmid"] = 101
			a.Entries = []map[string]any{entry}
		}, codes.FailedPrecondition},
		{"wrong format", func(a *testutil.VolumeAPI) {
			entry := testutil.VolumeEntry(a.Req)
			entry["format"] = "qcow2"
			a.Entries = []map[string]any{entry}
		}, codes.FailedPrecondition},
		{"wrong content", func(a *testutil.VolumeAPI) {
			entry := testutil.VolumeEntry(a.Req)
			entry["content"] = "rootdir"
			a.Entries = []map[string]any{entry}
		}, codes.FailedPrecondition},
		{"missing size", func(a *testutil.VolumeAPI) {
			entry := testutil.VolumeEntry(a.Req)
			delete(entry, "size")
			a.Entries = []map[string]any{entry}
		}, codes.FailedPrecondition},
		{"duplicate", func(a *testutil.VolumeAPI) {
			a.Entries = []map[string]any{testutil.VolumeEntry(a.Req), testutil.VolumeEntry(a.Req)}
		}, codes.Internal},
		{"missing ID", func(a *testutil.VolumeAPI) { a.Entries = []map[string]any{{"size": domain.GiB}} }, codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, client := newValidationAPI(t)
			api.Configure(func() { tc.setup(api) })
			_, err := client.ValidateVolumeCapabilities(testCSIContext(t), validationRequest())
			if status.Code(err) != tc.code || strings.Contains(err.Error(), testutil.FakeSecret) {
				t.Fatalf("backend error = %v, want %v", err, tc.code)
			}
			api.AssertCounts(t, 1, 0)
		})
	}
	api, client := newValidationAPI(t)
	for _, change := range []func(*domain.Request){
		func(r *domain.Request) { r.Node = "other" },
		func(r *domain.Request) { r.Storage = "other" },
		func(r *domain.Request) { r.OwnerVMID++ },
	} {
		foreign := testutil.VolumeRequest()
		change(&foreign)
		req := validationRequest()
		req.VolumeId = foreign.Handle()
		if _, err := client.ValidateVolumeCapabilities(testCSIContext(t), req); status.Code(err) != codes.NotFound {
			t.Fatalf("scope = %v", err)
		}
	}
	api.Mu.Lock()
	defer api.Mu.Unlock()
	if len(api.Calls) != 0 {
		t.Fatalf("foreign identity reached PVE: %v", api.Calls)
	}
}
