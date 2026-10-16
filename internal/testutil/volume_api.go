package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"pve-csi.local/csi-plugin/internal/volume/domain"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

const FakeTokenID = "test-user@pve!test-token"
const FakeSecret = "test-secret-do-not-print"

func CAFile(t *testing.T, cert []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const VolumeGiB int64 = 1 << 30

// The SHA-256 of "abc" is fixed here so the expected filename does not depend
// on the implementation's naming helper.
const VolumeNameHash = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

const VolumeStorageStatus = `{"data":{"type":"lvmthin","content":"images,rootdir","enabled":1,"active":1,"total":17179869184,"used":0,"avail":17179869184}}`

func VolumeRequest() domain.Request {
	return domain.Request{Name: "abc", Node: "test-pve", Storage: "test-pool", OwnerVMID: 482, CapacityBytes: 2 * VolumeGiB}
}

func VolumeFilename(req domain.Request) string {
	digest := sha256.Sum256([]byte(req.Name))
	return fmt.Sprintf("vm-%d-csi-%x", req.OwnerVMID, digest)
}

func VolumeID(req domain.Request) string {
	return req.Storage + ":" + VolumeFilename(req)
}

func VolumeEntry(req domain.Request) map[string]any {
	return map[string]any{
		"volid": VolumeID(req), "vmid": req.OwnerVMID, "format": "raw",
		"size": req.CapacityBytes, "content": "images",
	}
}

func ConfirmedVolume(req domain.Request) domain.Volume {
	volume, err := domain.Confirm(req, domain.Allocation{
		Reference: VolumeID(req), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes,
	})
	if err != nil {
		panic(err)
	}
	return volume
}

func AssertVolume(t *testing.T, got domain.Volume, req domain.Request) {
	t.Helper()
	want := ConfirmedVolume(req)
	if got != want {
		t.Errorf("volume = %+v, want %+v", got, want)
	}
}

type VolumeResponse struct {
	Status int
	Body   string
}

// This fake exposes storage operations only. Any attempt to attach a disk or
// inspect/modify a QEMU configuration fails the test through the default route.
type VolumeAPI struct {
	T                 *testing.T
	Req               domain.Request
	Mu                sync.Mutex
	Calls             []string
	ListCalls         int
	PostCalls         int
	Entries           []map[string]any
	StatusResponse    *VolumeResponse
	ListResponses     map[int]VolumeResponse
	PostResponse      *VolumeResponse
	PostLeavesMissing bool
	PostRedirect      string
	PostCancel        context.CancelFunc
}

func NewVolumeAPI(t *testing.T, req domain.Request) (*VolumeAPI, *pveapi.Client) {
	t.Helper()
	api := &VolumeAPI{T: t, Req: req, Entries: []map[string]any{}, ListResponses: make(map[int]VolumeResponse)}
	server := httptest.NewTLSServer(http.HandlerFunc(api.serveHTTP))
	t.Cleanup(server.Close)
	client, err := pveapi.NewClient(pveapi.Config{Endpoint: server.URL, TokenID: FakeTokenID, TokenSecret: FakeSecret, CAFile: CAFile(t, server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return api, client
}

func (api *VolumeAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	api.Mu.Lock()
	defer api.Mu.Unlock()
	api.Calls = append(api.Calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "PVEAPIToken="+FakeTokenID+"="+FakeSecret {
		api.T.Error("unexpected Authorization header")
	}
	if r.Header.Get("Accept") != "application/json" {
		api.T.Error("request does not accept JSON")
	}
	w.Header().Set("Content-Type", "application/json")
	base := "/api2/json/nodes/" + api.Req.Node + "/storage/" + api.Req.Storage
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base+"/status":
		if r.URL.RawQuery != "" {
			api.T.Errorf("unexpected status query: %s", r.URL.RawQuery)
		}
		response := VolumeResponse{http.StatusOK, VolumeStorageStatus}
		if api.StatusResponse != nil {
			response = *api.StatusResponse
		}
		api.writeResponse(w, response)
	case r.Method == http.MethodGet && r.URL.Path == base+"/content":
		api.ListCalls++
		want := url.Values{"content": {"images"}, "vmid": {strconv.Itoa(api.Req.OwnerVMID)}}
		if !reflect.DeepEqual(r.URL.Query(), want) {
			api.T.Errorf("content query = %v, want %v", r.URL.Query(), want)
		}
		if response, ok := api.ListResponses[api.ListCalls]; ok {
			api.writeResponse(w, response)
			return
		}
		api.writeData(w, api.Entries)
	case r.Method == http.MethodPost && r.URL.Path == base+"/content":
		api.PostCalls++
		if r.URL.RawQuery != "" {
			api.T.Errorf("unexpected POST query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			api.T.Errorf("POST Content-Type = %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			api.T.Errorf("parse allocation form: %v", err)
		}
		want := url.Values{
			"filename": {VolumeFilename(api.Req)}, "vmid": {strconv.Itoa(api.Req.OwnerVMID)},
			"size": {strconv.FormatInt(api.Req.CapacityBytes/1024, 10)}, "format": {"raw"},
		}
		if !reflect.DeepEqual(r.PostForm, want) {
			api.T.Errorf("allocation form = %v, want %v", r.PostForm, want)
		}
		if !api.PostLeavesMissing {
			api.Entries = append(api.Entries, VolumeEntry(api.Req))
		}
		if api.PostCancel != nil {
			api.PostCancel()
			return
		}
		if api.PostRedirect != "" {
			http.Redirect(w, r, api.PostRedirect, http.StatusTemporaryRedirect)
			return
		}
		if api.PostResponse != nil {
			api.writeResponse(w, *api.PostResponse)
			return
		}
		api.writeData(w, VolumeID(api.Req))
	default:
		api.T.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

func (api *VolumeAPI) writeResponse(w http.ResponseWriter, response VolumeResponse) {
	w.WriteHeader(response.Status)
	if _, err := fmt.Fprint(w, response.Body); err != nil {
		api.T.Errorf("write fake response: %v", err)
	}
}

func (api *VolumeAPI) writeData(w http.ResponseWriter, data any) {
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		api.T.Errorf("encode fake response: %v", err)
	}
}

func (api *VolumeAPI) Configure(change func()) {
	api.Mu.Lock()
	defer api.Mu.Unlock()
	change()
}

func (api *VolumeAPI) AssertCounts(t *testing.T, wantLists, wantPosts int) {
	t.Helper()
	api.Mu.Lock()
	defer api.Mu.Unlock()
	if api.ListCalls != wantLists || api.PostCalls != wantPosts {
		t.Errorf("list/POST calls = %d/%d, want %d/%d; calls: %v", api.ListCalls, api.PostCalls, wantLists, wantPosts, api.Calls)
	}
}
