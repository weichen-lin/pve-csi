package csidriver

import (
	"context"
	"errors"
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"math"
	"net"
	"net/http"
	"pve-csi.local/csi-plugin/internal/platform/csiserver"
	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"pve-csi.local/csi-plugin/internal/testutil"
	"pve-csi.local/csi-plugin/internal/volume/application"
	"pve-csi.local/csi-plugin/internal/volume/domain"
	"pve-csi.local/csi-plugin/internal/volume/infrastructure/pve"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testCSIAllocator func(context.Context, domain.Request) (domain.Volume, error)

func (f testCSIAllocator) EnsureVolume(ctx context.Context, req domain.Request) (domain.Volume, error) {
	return f(ctx, req)
}

func testCSIConfig() Config {
	return Config{Node: "test-pve", Storage: "test-pool", OwnerVMID: 482}
}

func testCSIRequest() *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name: "abc", CapacityRange: &csi.CapacityRange{RequiredBytes: testutil.VolumeGiB + 1},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	}
}

func testCSIVolume(req domain.Request) domain.Volume { return testutil.ConfirmedVolume(req) }

func newTestCSIController(t *testing.T, allocator application.Provisioner) *Controller {
	t.Helper()
	return newTestCSIControllerWithReader(t, allocator, testCSIReader(func(context.Context, domain.Identity) (domain.Volume, error) {
		t.Error("unexpected volume read")
		return domain.Volume{}, errors.New("unexpected volume read")
	}))
}

func newTestCSIControllerWithReader(t *testing.T, allocator application.Provisioner, reader application.VolumeReader) *Controller {
	t.Helper()
	coordinated, err := application.NewCoordinator(allocator)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(testCSIConfig(), coordinated, reader)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func testCSIContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newTestCSIConnection(t *testing.T, allocator application.Provisioner) *grpc.ClientConn {
	t.Helper()
	return connectTestCSIController(t, newTestCSIController(t, allocator))
}

func connectTestCSIController(t *testing.T, controller *Controller) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	csiserver.Register(server, controller, "dev-005")
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		if err := <-serveDone; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("gRPC server stopped unexpectedly: %v", err)
		}
	})
	connection, err := grpc.NewClient("passthrough:///csi-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func TestCSICreateVolumeGRPCRetryAndRestart(t *testing.T) {
	// Derive expected backend input from an independent, fixed hash vector.
	backendReq := testutil.VolumeRequest()
	backendReq.Name = "csi-v1-" + testutil.VolumeNameHash
	api, pveClient := testutil.NewVolumeAPI(t, backendReq)
	allocator := testPVEProvisioner(t, pveClient)
	client := csi.NewControllerClient(newTestCSIConnection(t, allocator))
	ctx := testCSIContext(t)
	first, err := client.CreateVolume(ctx, testCSIRequest())
	if err != nil {
		t.Fatal(err)
	}
	wantID := "v1:test-pve:test-pool:" + testutil.VolumeFilename(backendReq)
	if first.GetVolume().GetVolumeId() != wantID || first.GetVolume().GetCapacityBytes() != 2*testutil.VolumeGiB {
		t.Fatalf("unexpected volume: %v", first.GetVolume())
	}
	topology := first.Volume.AccessibleTopology
	if len(topology) != 1 || len(topology[0].Segments) != 1 || topology[0].Segments[pveNodeTopologyKey] != "test-pve" {
		t.Fatalf("unexpected accessibility: %v", topology)
	}
	second, err := client.CreateVolume(ctx, testCSIRequest())
	if err != nil || second.GetVolume().GetVolumeId() != wantID {
		t.Fatalf("repeat = %v, %v", second, err)
	}
	// A fresh Controller has no knowledge of the first one's memory. PVE's
	// stored identity and size must be enough to return the same volume.
	restarted := csi.NewControllerClient(newTestCSIConnection(t, allocator))
	broader := testCSIRequest()
	broader.CapacityRange = &csi.CapacityRange{RequiredBytes: testutil.VolumeGiB, LimitBytes: 3 * testutil.VolumeGiB}
	broader.Parameters = map[string]string{"storage": "test-pool"}
	third, err := restarted.CreateVolume(ctx, broader)
	if err != nil || third.GetVolume().GetVolumeId() != wantID || third.GetVolume().GetCapacityBytes() != 2*testutil.VolumeGiB {
		t.Fatalf("restart/range reuse = %v, %v", third, err)
	}
	tooLarge := testCSIRequest()
	tooLarge.CapacityRange.RequiredBytes = 3 * testutil.VolumeGiB
	_, err = restarted.CreateVolume(ctx, tooLarge)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("conflict = %v", err)
	}
	api.AssertCounts(t, 5, 1)
}

func TestCSICapacity(t *testing.T) {
	for _, tc := range []struct {
		name            string
		input           *csi.CapacityRange
		capacity, limit int64
		code            codes.Code
	}{
		{"default", nil, testutil.VolumeGiB, math.MaxInt64, codes.OK},
		{"small", &csi.CapacityRange{RequiredBytes: 1}, testutil.VolumeGiB, math.MaxInt64, codes.OK},
		{"round up", &csi.CapacityRange{RequiredBytes: testutil.VolumeGiB + 1}, 2 * testutil.VolumeGiB, math.MaxInt64, codes.OK},
		{"exact", &csi.CapacityRange{RequiredBytes: 2 * testutil.VolumeGiB, LimitBytes: 2 * testutil.VolumeGiB}, 2 * testutil.VolumeGiB, 2 * testutil.VolumeGiB, codes.OK},
		{"only limit", &csi.CapacityRange{LimitBytes: 3 * testutil.VolumeGiB}, testutil.VolumeGiB, 3 * testutil.VolumeGiB, codes.OK},
		{"largest aligned", &csi.CapacityRange{RequiredBytes: math.MaxInt64 / testutil.VolumeGiB * testutil.VolumeGiB}, math.MaxInt64 / testutil.VolumeGiB * testutil.VolumeGiB, math.MaxInt64, codes.OK},
		{"empty", &csi.CapacityRange{}, 0, 0, codes.InvalidArgument},
		{"negative required", &csi.CapacityRange{RequiredBytes: -1}, 0, 0, codes.InvalidArgument},
		{"negative limit", &csi.CapacityRange{LimitBytes: -1}, 0, 0, codes.InvalidArgument},
		{"inverted", &csi.CapacityRange{RequiredBytes: 3, LimitBytes: 2}, 0, 0, codes.InvalidArgument},
		{"limit below minimum", &csi.CapacityRange{LimitBytes: testutil.VolumeGiB - 1}, 0, 0, codes.OutOfRange},
		{"unaligned exact", &csi.CapacityRange{RequiredBytes: testutil.VolumeGiB + 1, LimitBytes: testutil.VolumeGiB + 1}, 0, 0, codes.OutOfRange},
		{"overflow", &csi.CapacityRange{RequiredBytes: math.MaxInt64}, 0, 0, codes.OutOfRange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capacity, limit, err := csiCapacity(tc.input)
			if status.Code(err) != tc.code || capacity != tc.capacity || limit != tc.limit {
				t.Fatalf("got %d, %d, %v; want %d, %d, %v", capacity, limit, err, tc.capacity, tc.limit, tc.code)
			}
		})
	}
}

func TestCSIRejectsInvalidRequestsBeforeBackend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*csi.CreateVolumeRequest)
	}{
		{"empty name", func(r *csi.CreateVolumeRequest) { r.Name = "" }},
		{"long name", func(r *csi.CreateVolumeRequest) { r.Name = strings.Repeat("a", 129) }},
		{"unicode byte length", func(r *csi.CreateVolumeRequest) { r.Name = strings.Repeat("碟", 43) }},
		{"invalid UTF-8", func(r *csi.CreateVolumeRequest) { r.Name = "\xff" }},
		{"control character", func(r *csi.CreateVolumeRequest) { r.Name = "abc\x00" }},
		{"no capabilities", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities = nil }},
		{"nil capability", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities = []*csi.VolumeCapability{nil} }},
		{"no access type", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities[0].AccessType = nil }},
		{"block", func(r *csi.CreateVolumeRequest) {
			r.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
		}},
		{"no access mode", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities[0].AccessMode = nil }},
		{"multi node", func(r *csi.CreateVolumeRequest) {
			r.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		}},
		{"filesystem", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities[0].GetMount().FsType = "xfs" }},
		{"mount flags", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities[0].GetMount().MountFlags = []string{"discard"} }},
		{"mount group", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities[0].GetMount().VolumeMountGroup = "1000" }},
		{"invalid second capability", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities = append(r.VolumeCapabilities, nil) }},
		{"content source", func(r *csi.CreateVolumeRequest) { r.VolumeContentSource = &csi.VolumeContentSource{} }},
		{"mutable parameters", func(r *csi.CreateVolumeRequest) { r.MutableParameters = map[string]string{"key": testutil.FakeSecret} }},
		{"secrets", func(r *csi.CreateVolumeRequest) { r.Secrets = map[string]string{"token": testutil.FakeSecret} }},
		{"unknown parameter", func(r *csi.CreateVolumeRequest) { r.Parameters = map[string]string{"owner": "101"} }},
		{"different storage", func(r *csi.CreateVolumeRequest) { r.Parameters = map[string]string{"storage": "other-pool"} }},
		{"empty capacity range", func(r *csi.CreateVolumeRequest) { r.CapacityRange = &csi.CapacityRange{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := newTestCSIController(t, testCSIAllocator(func(context.Context, domain.Request) (domain.Volume, error) {
				t.Error("invalid request reached backend")
				return domain.Volume{}, errors.New(testutil.FakeSecret)
			}))
			req := testCSIRequest()
			tc.change(req)
			_, err := controller.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument || strings.Contains(err.Error(), testutil.FakeSecret) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	controller := newTestCSIController(t, testCSIAllocator(func(context.Context, domain.Request) (domain.Volume, error) {
		t.Error("nil request reached backend")
		return domain.Volume{}, nil
	}))
	if _, err := controller.CreateVolume(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil request: %v", err)
	}
}

func TestCSINamesPreserveBytesAndDefaultFilesystem(t *testing.T) {
	seen := make(map[string]bool)
	controller := newTestCSIController(t, testCSIAllocator(func(_ context.Context, req domain.Request) (domain.Volume, error) {
		if err := req.Validate(); err != nil {
			t.Errorf("normalized name rejected: %v", err)
		}
		if seen[req.Name] {
			t.Errorf("distinct names collapsed into %s", req.Name)
		}
		seen[req.Name] = true
		return testCSIVolume(req), nil
	}))
	for _, name := range []string{"abc", " abc", "abc ", "abc\t", "abc\n", "abc\r", "碟", " "} {
		req := testCSIRequest()
		req.Name = name
		req.VolumeCapabilities[0].GetMount().FsType = ""
		if _, err := controller.CreateVolume(context.Background(), req); err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
	}
}

func TestCSITopology(t *testing.T) {
	node := func(name string) *csi.Topology {
		return &csi.Topology{Segments: map[string]string{pveNodeTopologyKey: name}}
	}
	for _, tc := range []struct {
		name     string
		topology *csi.TopologyRequirement
		code     codes.Code
	}{
		{"omitted", nil, codes.OK},
		{"empty requirement", &csi.TopologyRequirement{}, codes.InvalidArgument},
		{"matching", &csi.TopologyRequirement{Requisite: []*csi.Topology{node("test-pve")}}, codes.OK},
		{"multiple choices", &csi.TopologyRequirement{Requisite: []*csi.Topology{node("other-pve"), node("test-pve")}}, codes.OK},
		{"preferred is advisory", &csi.TopologyRequirement{Preferred: []*csi.Topology{node("other-pve")}}, codes.OK},
		{"requisite mismatch", &csi.TopologyRequirement{Requisite: []*csi.Topology{node("other-pve")}}, codes.ResourceExhausted},
		{"preferred cannot override requisite", &csi.TopologyRequirement{Requisite: []*csi.Topology{node("other-pve")}, Preferred: []*csi.Topology{node("test-pve")}}, codes.ResourceExhausted},
		{"nil topology", &csi.TopologyRequirement{Requisite: []*csi.Topology{nil}}, codes.InvalidArgument},
		{"empty topology", &csi.TopologyRequirement{Requisite: []*csi.Topology{{}}}, codes.InvalidArgument},
		{"unknown key", &csi.TopologyRequirement{Requisite: []*csi.Topology{{Segments: map[string]string{"zone": "test-pve"}}}}, codes.InvalidArgument},
		{"invalid node", &csi.TopologyRequirement{Preferred: []*csi.Topology{node("../pve")}}, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := newTestCSIController(t, testCSIAllocator(func(_ context.Context, req domain.Request) (domain.Volume, error) {
				if tc.code != codes.OK {
					t.Error("invalid topology reached backend")
				}
				return testCSIVolume(req), nil
			}))
			req := testCSIRequest()
			req.AccessibilityRequirements = tc.topology
			if _, err := controller.CreateVolume(context.Background(), req); status.Code(err) != tc.code {
				t.Fatalf("error = %v, want %v", err, tc.code)
			}
		})
	}
}

func TestCSIConfiguration(t *testing.T) {
	allocator := testCSIAllocator(func(_ context.Context, req domain.Request) (domain.Volume, error) { return testCSIVolume(req), nil })
	reader := testCSIReader(func(context.Context, domain.Identity) (domain.Volume, error) { return domain.Volume{}, nil })
	if _, err := NewController(testCSIConfig(), nil, reader); err == nil {
		t.Fatal("nil allocator accepted")
	}
	if _, err := NewController(testCSIConfig(), allocator, nil); err == nil {
		t.Fatal("nil reader accepted")
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Node = "../pve" },
		func(c *Config) { c.Storage = "pool:other" },
		func(c *Config) { c.OwnerVMID = 0 },
		func(c *Config) { c.Node = strings.Repeat("n", 63) },
	} {
		cfg := testCSIConfig()
		change(&cfg)
		if _, err := NewController(cfg, allocator, reader); err == nil {
			t.Errorf("invalid configuration accepted: %+v", cfg)
		}
	}
}

func TestCSIBackendErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"canceled", context.Canceled, codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"canceled unconfirmed allocation", errors.Join(application.ErrUnconfirmed, context.Canceled), codes.Canceled},
		{"deadline unconfirmed allocation", errors.Join(application.ErrUnconfirmed, context.DeadlineExceeded), codes.DeadlineExceeded},
		{"unconfirmed readback conflict", errors.Join(application.ErrUnconfirmed, domain.ErrConflict), codes.Unavailable},
		{"conflict", domain.ErrConflict, codes.AlreadyExists},
		{"capacity", application.ErrCapacity, codes.ResourceExhausted},
		{"storage", application.ErrStorage, codes.FailedPrecondition},
		{"unconfirmed hides readback error", errors.Join(application.ErrUnconfirmed, application.NewBackendError(application.FailurePermissionDenied, errors.New(testutil.FakeSecret))), codes.Unavailable},
		{"unauthenticated", application.NewBackendError(application.FailureUnauthenticated, errors.New(testutil.FakeSecret)), codes.Unauthenticated},
		{"forbidden", application.NewBackendError(application.FailurePermissionDenied, errors.New(testutil.FakeSecret)), codes.PermissionDenied},
		{"missing", application.NewBackendError(application.FailureNotFound, errors.New(testutil.FakeSecret)), codes.FailedPrecondition},
		{"rate limited", application.NewBackendError(application.FailureUnavailable, errors.New(testutil.FakeSecret)), codes.Unavailable},
		{"server failure", application.NewBackendError(application.FailureUnavailable, errors.New(testutil.FakeSecret)), codes.Unavailable},
		{"other HTTP failure", application.NewBackendError(application.FailureInternal, errors.New(testutil.FakeSecret)), codes.Internal},
		{"unknown backend", errors.New(testutil.FakeSecret), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := newTestCSIController(t, testCSIAllocator(func(context.Context, domain.Request) (domain.Volume, error) {
				return domain.Volume{}, fmt.Errorf("%s: %w", testutil.FakeSecret, tc.err)
			}))
			_, err := controller.CreateVolume(context.Background(), testCSIRequest())
			if status.Code(err) != tc.code || strings.Contains(err.Error(), testutil.FakeSecret) {
				t.Fatalf("error = %v, want %v without secret", err, tc.code)
			}
		})
	}
}

func TestCSIHTTPFailureThroughGRPC(t *testing.T) {
	backendReq := testutil.VolumeRequest()
	backendReq.Name = "csi-v1-" + testutil.VolumeNameHash
	api, pveClient := testutil.NewVolumeAPI(t, backendReq)
	api.Configure(func() { api.ListResponses[1] = testutil.VolumeResponse{http.StatusForbidden, testutil.FakeSecret} })
	client := csi.NewControllerClient(newTestCSIConnection(t, testPVEProvisioner(t, pveClient)))
	_, err := client.CreateVolume(testCSIContext(t), testCSIRequest())
	if status.Code(err) != codes.PermissionDenied || strings.Contains(err.Error(), testutil.FakeSecret) {
		t.Fatalf("error = %v", err)
	}
	api.AssertCounts(t, 1, 0)
}

func TestCSIRejectsInconsistentBackendResult(t *testing.T) {
	for _, change := range []func(*domain.Request){
		func(r *domain.Request) { r.Name = "other-volume" },
		func(r *domain.Request) { r.Node = "other" },
		func(r *domain.Request) { r.Storage = "other" },
		func(r *domain.Request) { r.OwnerVMID = 101 },
		func(r *domain.Request) { r.CapacityBytes = testutil.VolumeGiB },
		func(r *domain.Request) { r.CapacityBytes = 4 * testutil.VolumeGiB; r.CapacityLimitBytes = 0 },
	} {
		controller := newTestCSIController(t, testCSIAllocator(func(_ context.Context, req domain.Request) (domain.Volume, error) {
			change(&req)
			return testCSIVolume(req), nil
		}))
		req := testCSIRequest()
		req.CapacityRange.LimitBytes = 3 * testutil.VolumeGiB
		if _, err := controller.CreateVolume(context.Background(), req); status.Code(err) != codes.Internal {
			t.Fatalf("inconsistent result: %v", err)
		}
	}
}

func TestCSIConcurrentSameName(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	allocator := testCSIAllocator(func(ctx context.Context, req domain.Request) (domain.Volume, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return domain.Volume{}, ctx.Err()
			}
		}
		return testCSIVolume(req), nil
	})
	client := csi.NewControllerClient(newTestCSIConnection(t, allocator))
	ctx := testCSIContext(t)
	firstDone := make(chan error, 1)
	go func() { _, err := client.CreateVolume(ctx, testCSIRequest()); firstDone <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first request did not enter backend")
	}
	_, err := client.CreateVolume(ctx, testCSIRequest())
	if status.Code(err) != codes.Aborted || calls.Load() != 1 {
		t.Fatalf("duplicate request: %v, backend calls %d", err, calls.Load())
	}
	// Unrelated names can proceed while the first RPC is blocked.
	other := testCSIRequest()
	other.Name = "other-volume"
	if _, err := client.CreateVolume(ctx, other); err != nil {
		t.Fatalf("different name blocked: %v", err)
	}
	unblock()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("first request did not finish")
	}
	if _, err := client.CreateVolume(ctx, testCSIRequest()); err != nil {
		t.Fatalf("retry after completion: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("backend calls = %d, want 3", calls.Load())
	}
}

func TestCSIGuardReleasedAfterErrorAndCancellation(t *testing.T) {
	var calls int
	controller := newTestCSIController(t, testCSIAllocator(func(_ context.Context, req domain.Request) (domain.Volume, error) {
		calls++
		if calls == 1 {
			return domain.Volume{}, errors.New("injected failure")
		}
		if calls == 2 {
			return domain.Volume{}, context.Canceled
		}
		return testCSIVolume(req), nil
	}))
	for _, code := range []codes.Code{codes.Internal, codes.Canceled, codes.OK} {
		_, err := controller.CreateVolume(context.Background(), testCSIRequest())
		if status.Code(err) != code {
			t.Fatalf("retry error = %v, want %v", err, code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := controller.CreateVolume(ctx, testCSIRequest()); status.Code(err) != codes.Canceled || calls != 3 {
		t.Fatalf("canceled request reached backend: calls %d, %v", calls, err)
	}
}

func TestCSIDiscoveryAndUnimplementedOperations(t *testing.T) {
	connection := newTestCSIConnection(t, testCSIAllocator(func(context.Context, domain.Request) (domain.Volume, error) {
		t.Error("discovery reached volume backend")
		return domain.Volume{}, nil
	}))
	ctx := testCSIContext(t)
	identity := csi.NewIdentityClient(connection)
	info, err := identity.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
	if err != nil || info.GetName() != csiserver.DriverName || info.GetVendorVersion() != "dev-005" {
		t.Fatalf("info = %v, %v", info, err)
	}
	capabilities, err := identity.GetPluginCapabilities(ctx, &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[csi.PluginCapability_Service_Type]bool)
	for _, capability := range capabilities.Capabilities {
		seen[capability.GetService().GetType()] = true
	}
	if len(seen) != 2 || !seen[csi.PluginCapability_Service_CONTROLLER_SERVICE] || !seen[csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS] {
		t.Fatalf("capabilities = %v", capabilities)
	}
	probe, err := identity.Probe(ctx, &csi.ProbeRequest{})
	if err != nil || !probe.GetReady().GetValue() {
		t.Fatalf("probe = %v, %v", probe, err)
	}
	controller := csi.NewControllerClient(connection)
	caps, err := controller.ControllerGetCapabilities(ctx, &csi.ControllerGetCapabilitiesRequest{})
	if err != nil || len(caps.GetCapabilities()) != 0 {
		t.Fatalf("premature controller capabilities: %v, %v", caps, err)
	}
	if _, err := controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "unused"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("delete = %v", err)
	}
	if _, err := controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: "unused", NodeId: "unused"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("publish = %v", err)
	}
}

func testPVEProvisioner(t *testing.T, client *pveapi.Client) application.Provisioner {
	t.Helper()
	gateway, err := pve.NewStorageGateway(client)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(gateway)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
