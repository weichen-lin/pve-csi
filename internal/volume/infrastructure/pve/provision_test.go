package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"pve-csi.local/csi-plugin/internal/testutil"
	"pve-csi.local/csi-plugin/internal/volume/application"
	"pve-csi.local/csi-plugin/internal/volume/domain"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func newVolumeService(t *testing.T, req domain.Request) (*testutil.VolumeAPI, *application.Service) {
	t.Helper()
	api, client := testutil.NewVolumeAPI(t, req)
	gateway, err := NewStorageGateway(client)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(gateway)
	if err != nil {
		t.Fatal(err)
	}
	return api, service
}

func TestEnsureVolumeCreatesAndReuses(t *testing.T) {
	req := testutil.VolumeRequest()
	if got := req.Filename(); got != "vm-482-csi-"+testutil.VolumeNameHash {
		t.Fatalf("filename does not match the independent SHA-256 vector: %s", got)
	}
	api, service := newVolumeService(t, req)
	// Existing system disks are unrelated to the requested independent volume.
	api.Configure(func() {
		api.Entries = []map[string]any{{"volid": req.Storage + ":vm-482-disk-0", "vmid": 482, "format": "raw", "size": 64 * testutil.VolumeGiB, "content": "images"}}
	})
	// The allocation must use its explicit inputs, not query-mode VM settings.
	// The gateway connection has no query VM or query storage fields.
	for attempt := 1; attempt <= 2; attempt++ {
		got, err := service.EnsureVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		testutil.AssertVolume(t, got, req)
	}
	api.AssertCounts(t, 3, 1)
	api.Mu.Lock()
	defer api.Mu.Unlock()
	base := "/api2/json/nodes/" + req.Node + "/storage/" + req.Storage
	wantCalls := []string{
		"GET " + base + "/status", "GET " + base + "/content", "POST " + base + "/content", "GET " + base + "/content",
		"GET " + base + "/status", "GET " + base + "/content",
	}
	if !reflect.DeepEqual(api.Calls, wantCalls) {
		t.Errorf("requests = %v, want %v", api.Calls, wantCalls)
	}
	if len(api.Entries) != 2 || api.Entries[0]["volid"] != req.Storage+":vm-482-disk-0" {
		t.Error("allocation did not preserve the unrelated system disk")
	}
}

func TestEnsureVolumeReusesExistingWithNoFreeCapacity(t *testing.T) {
	req := testutil.VolumeRequest()
	api, service := newVolumeService(t, req)
	api.Configure(func() {
		api.Entries = []map[string]any{testutil.VolumeEntry(req)}
		api.StatusResponse = &testutil.VolumeResponse{http.StatusOK, strings.Replace(testutil.VolumeStorageStatus, `"avail":17179869184`, `"avail":0`, 1)}
	})
	got, err := service.EnsureVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertVolume(t, got, req)
	api.AssertCounts(t, 1, 0)
}

func TestEnsureVolumeCapacityRange(t *testing.T) {
	for _, tc := range []struct {
		name         string
		size         int64
		wantConflict bool
	}{
		{"lower bound", 2 * testutil.VolumeGiB, false},
		{"larger compatible", 3 * testutil.VolumeGiB, false},
		{"upper bound", 4 * testutil.VolumeGiB, false},
		{"too small", testutil.VolumeGiB, true},
		{"too large", 5 * testutil.VolumeGiB, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			req.CapacityLimitBytes = 4 * testutil.VolumeGiB
			api, service := newVolumeService(t, req)
			entry := testutil.VolumeEntry(req)
			entry["size"] = tc.size
			api.Configure(func() { api.Entries = []map[string]any{entry} })
			got, err := service.EnsureVolume(context.Background(), req)
			if tc.wantConflict {
				if !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("error = %v, want conflict", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got.CapacityBytes() != tc.size {
					t.Errorf("size = %d, want %d", got.CapacityBytes(), tc.size)
				}
			}
			api.AssertCounts(t, 1, 0)
		})
	}
}

func TestEnsureVolumeChangedCapacityDoesNotAllocateAgain(t *testing.T) {
	req := testutil.VolumeRequest()
	api, service := newVolumeService(t, req)
	if _, err := service.EnsureVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.CapacityBytes = 3 * testutil.VolumeGiB
	if _, err := service.EnsureVolume(context.Background(), req); err == nil {
		t.Error("retry with changed capacity accepted")
	}
	api.AssertCounts(t, 3, 1)
}

func TestEnsureVolumeRejectsExistingConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(map[string]any)
		duplicate bool
	}{
		{"different capacity", func(entry map[string]any) { entry["size"] = 3 * testutil.VolumeGiB }, false},
		{"different owner", func(entry map[string]any) { entry["vmid"] = 483 }, false},
		{"different format", func(entry map[string]any) { entry["format"] = "qcow2" }, false},
		{"different content", func(entry map[string]any) { entry["content"] = "rootdir" }, false},
		{"missing owner", func(entry map[string]any) { delete(entry, "vmid") }, false},
		{"missing size", func(entry map[string]any) { delete(entry, "size") }, false},
		{"missing format", func(entry map[string]any) { delete(entry, "format") }, false},
		{"missing content", func(entry map[string]any) { delete(entry, "content") }, false},
		{"missing volume ID", func(entry map[string]any) { delete(entry, "volid") }, false},
		{"duplicate identity", func(map[string]any) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			api, service := newVolumeService(t, req)
			entry := testutil.VolumeEntry(req)
			tc.change(entry)
			api.Configure(func() {
				api.Entries = []map[string]any{entry}
				if tc.duplicate {
					api.Entries = append(api.Entries, testutil.VolumeEntry(req))
				}
			})
			if _, err := service.EnsureVolume(context.Background(), req); err == nil {
				t.Error("existing incompatible or duplicate volume accepted")
			}
			api.AssertCounts(t, 1, 0)
		})
	}
}

func TestEnsureVolumeRejectsUnavailableStorage(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"unsupported backend", `"type":"lvmthin"`, `"type":"dir"`},
		{"disabled", `"enabled":1`, `"enabled":0`},
		{"inactive", `"active":1`, `"active":0`},
		{"no images content", `"content":"images,rootdir"`, `"content":"rootdir"`},
		{"content substring", `"content":"images,rootdir"`, `"content":"notimages"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			api, service := newVolumeService(t, req)
			api.Configure(func() {
				api.StatusResponse = &testutil.VolumeResponse{http.StatusOK, strings.Replace(testutil.VolumeStorageStatus, tc.old, tc.replacement, 1)}
			})
			if _, err := service.EnsureVolume(context.Background(), req); err == nil {
				t.Error("unsuitable storage accepted")
			}
			api.AssertCounts(t, 0, 0)
		})
	}
}

func TestEnsureVolumeRejectsInsufficientCapacity(t *testing.T) {
	req := testutil.VolumeRequest()
	api, service := newVolumeService(t, req)
	api.Configure(func() {
		api.StatusResponse = &testutil.VolumeResponse{http.StatusOK, strings.Replace(testutil.VolumeStorageStatus, `"avail":17179869184`, `"avail":1073741824`, 1)}
	})
	if _, err := service.EnsureVolume(context.Background(), req); err == nil {
		t.Error("allocation beyond available capacity accepted")
	}
	api.AssertCounts(t, 1, 0)
}

func TestEnsureVolumeRejectsInvalidRequestsBeforeNetwork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.Request)
	}{
		{"empty name", func(req *domain.Request) { req.Name = "" }},
		{"blank name", func(req *domain.Request) { req.Name = " \t" }},
		{"leading name whitespace", func(req *domain.Request) { req.Name = " abc" }},
		{"trailing name whitespace", func(req *domain.Request) { req.Name = "abc " }},
		{"name control character", func(req *domain.Request) { req.Name = "abc\x00def" }},
		{"name line break", func(req *domain.Request) { req.Name = "abc\ndef" }},
		{"name too long", func(req *domain.Request) { req.Name = strings.Repeat("a", 129) }},
		{"name byte limit", func(req *domain.Request) { req.Name = strings.Repeat("碟", 43) }},
		{"empty node", func(req *domain.Request) { req.Node = "" }},
		{"node path traversal", func(req *domain.Request) { req.Node = "../test-pve" }},
		{"node query", func(req *domain.Request) { req.Node = "test-pve?x=y" }},
		{"node leading dash", func(req *domain.Request) { req.Node = "-test-pve" }},
		{"node trailing dash", func(req *domain.Request) { req.Node = "test-pve-" }},
		{"node too long", func(req *domain.Request) { req.Node = strings.Repeat("a", 64) }},
		{"empty storage", func(req *domain.Request) { req.Storage = "" }},
		{"storage path traversal", func(req *domain.Request) { req.Storage = "../test-pool" }},
		{"storage query", func(req *domain.Request) { req.Storage = "test-pool?x=y" }},
		{"storage leading digit", func(req *domain.Request) { req.Storage = "1pool" }},
		{"storage trailing dash", func(req *domain.Request) { req.Storage = "pool-" }},
		{"owner too small", func(req *domain.Request) { req.OwnerVMID = 99 }},
		{"owner too large", func(req *domain.Request) { req.OwnerVMID = 1000000000 }},
		{"zero capacity", func(req *domain.Request) { req.CapacityBytes = 0 }},
		{"negative capacity", func(req *domain.Request) { req.CapacityBytes = -testutil.VolumeGiB }},
		{"negative limit", func(req *domain.Request) { req.CapacityLimitBytes = -1 }},
		{"limit below required", func(req *domain.Request) { req.CapacityLimitBytes = testutil.VolumeGiB }},
		{"unaligned capacity", func(req *domain.Request) { req.CapacityBytes = testutil.VolumeGiB + 1 }},
		{"capacity exceeds largest GiB multiple", func(req *domain.Request) { req.CapacityBytes = 1<<63 - 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			api, service := newVolumeService(t, req)
			tc.change(&req)
			if _, err := service.EnsureVolume(context.Background(), req); err == nil {
				t.Error("invalid request accepted")
			}
			api.Mu.Lock()
			defer api.Mu.Unlock()
			if len(api.Calls) != 0 {
				t.Errorf("invalid request reached network: %v", api.Calls)
			}
		})
	}
}

func TestEnsureVolumeListFailureDoesNotAllocate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response testutil.VolumeResponse
	}{
		{"forbidden", testutil.VolumeResponse{http.StatusForbidden, testutil.FakeSecret}},
		{"non-200 success status", testutil.VolumeResponse{http.StatusCreated, `{"data":[]}`}},
		{"invalid JSON", testutil.VolumeResponse{http.StatusOK, testutil.FakeSecret}},
		{"null list", testutil.VolumeResponse{http.StatusOK, `{"data":null}`}},
		{"wrong shape", testutil.VolumeResponse{http.StatusOK, `{"data":{}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			api, service := newVolumeService(t, req)
			api.Configure(func() { api.ListResponses[1] = tc.response })
			_, err := service.EnsureVolume(context.Background(), req)
			if err == nil {
				t.Fatal("failed list treated as an empty list")
			}
			if strings.Contains(err.Error(), testutil.FakeSecret) {
				t.Error("error contains response body secret")
			}
			api.AssertCounts(t, 1, 0)
		})
	}
}

func TestEnsureVolumeRecoversAllocationByReadback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response testutil.VolumeResponse
	}{
		{"server failure after allocation", testutil.VolumeResponse{http.StatusInternalServerError, testutil.FakeSecret}},
		{"invalid response JSON", testutil.VolumeResponse{http.StatusOK, testutil.FakeSecret}},
		{"wrong response shape", testutil.VolumeResponse{http.StatusOK, `{"data":{"unexpected":true}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			api, service := newVolumeService(t, req)
			api.Configure(func() { api.PostResponse = &tc.response })
			got, err := service.EnsureVolume(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			testutil.AssertVolume(t, got, req)
			api.AssertCounts(t, 2, 1)
		})
	}
}

func TestEnsureVolumeWrongSuccessIDRemainsUnconfirmed(t *testing.T) {
	for _, tc := range []struct{ name, returnedID string }{
		{"different volume", "unrelated-pool:wrong-volume"},
		{"untrusted response redaction", testutil.FakeSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			api, service := newVolumeService(t, req)
			body, err := json.Marshal(map[string]any{"data": tc.returnedID})
			if err != nil {
				t.Fatal(err)
			}
			api.Configure(func() { api.PostResponse = &testutil.VolumeResponse{http.StatusOK, string(body)} })
			// The expected volume exists after POST, but the explicit different
			// success ID still indicates a protocol inconsistency to investigate.
			_, err = service.EnsureVolume(context.Background(), req)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unconfirmed") {
				t.Fatalf("wrong success ID was accepted: %v", err)
			}
			if strings.Contains(err.Error(), tc.returnedID) {
				t.Error("error echoes untrusted returned volume ID")
			}
			api.AssertCounts(t, 2, 1)
		})
	}
}

func TestEnsureVolumeReportsUnconfirmedAllocation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *testutil.VolumeResponse
		readback *testutil.VolumeResponse
	}{
		{"missing after successful POST", nil, nil},
		{"wrong returned ID and absent", &testutil.VolumeResponse{http.StatusOK, `{"data":"unrelated-pool:wrong-volume"}`}, nil},
		{"failed POST and absent", &testutil.VolumeResponse{http.StatusInternalServerError, testutil.FakeSecret}, nil},
		{"HTTP 201 is not API success", &testutil.VolumeResponse{http.StatusCreated, `{"data":"ignored"}`}, nil},
		{"readback denied", nil, &testutil.VolumeResponse{http.StatusForbidden, testutil.FakeSecret}},
		{"readback capacity conflict", nil, &testutil.VolumeResponse{http.StatusOK, fmt.Sprintf(`{"data":[{"volid":"test-pool:vm-482-csi-%s","vmid":482,"format":"raw","content":"images","size":3221225472}]}`, testutil.VolumeNameHash)}},
		{"failed POST and denied readback", &testutil.VolumeResponse{http.StatusInternalServerError, testutil.FakeSecret}, &testutil.VolumeResponse{http.StatusForbidden, testutil.FakeSecret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.VolumeRequest()
			api, service := newVolumeService(t, req)
			api.Configure(func() {
				api.PostLeavesMissing = true
				api.PostResponse = tc.response
				if tc.readback != nil {
					api.ListResponses[2] = *tc.readback
				}
			})
			_, err := service.EnsureVolume(context.Background(), req)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unconfirmed") {
				t.Fatalf("expected unconfirmed allocation, got %v", err)
			}
			if strings.Contains(err.Error(), testutil.FakeSecret) {
				t.Error("error contains response body secret")
			}
			api.AssertCounts(t, 2, 1)
		})
	}
}

func TestEnsureVolumeCanceledBeforeNetwork(t *testing.T) {
	req := testutil.VolumeRequest()
	api, service := newVolumeService(t, req)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.EnsureVolume(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	api.Mu.Lock()
	defer api.Mu.Unlock()
	if len(api.Calls) != 0 {
		t.Errorf("canceled operation reached server: %v", api.Calls)
	}
}

func TestEnsureVolumeCancellationAfterAllocationDoesNotRetry(t *testing.T) {
	req := testutil.VolumeRequest()
	api, service := newVolumeService(t, req)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.Configure(func() { api.PostCancel = cancel })
	_, err := service.EnsureVolume(ctx, req)
	if !errors.Is(err, context.Canceled) || !strings.Contains(strings.ToLower(err.Error()), "unconfirmed") {
		t.Fatalf("expected unconfirmed allocation and context cancellation, got %v", err)
	}
	// The readback shares the canceled caller context and cannot reach the API.
	api.AssertCounts(t, 1, 1)
	api.Mu.Lock()
	defer api.Mu.Unlock()
	if len(api.Entries) != 1 {
		t.Fatal("fake allocation did not occur before cancellation")
	}
}

func TestEnsureVolumeRejectsPOSTRedirect(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		fmt.Fprint(w, `{"data":"unexpected"}`)
	}))
	defer destination.Close()
	req := testutil.VolumeRequest()
	api, service := newVolumeService(t, req)
	api.Configure(func() {
		api.PostLeavesMissing = true
		api.PostRedirect = destination.URL
	})
	_, err := service.EnsureVolume(context.Background(), req)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unconfirmed") {
		t.Fatalf("expected unconfirmed redirected allocation, got %v", err)
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("POST redirect was followed, potentially forwarding credentials")
	}
	api.AssertCounts(t, 2, 1)
}
