package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

const checkTestSecret = "fake-token-secret-never-print"

type checkFixture struct {
	permissions map[string]map[string]int
	configs     map[int]map[string]any
	nodes       []map[string]string
	inventory   []map[string]int
	storage     map[string]any
	pending     map[int][]map[string]any
	ownerInUse  bool
	override    func(http.ResponseWriter, *http.Request) bool
	mu          sync.Mutex
	requests    []string
}

func newCheckFixture() *checkFixture {
	return &checkFixture{
		permissions: map[string]map[string]int{
			"/nodes/pve":    {"Sys.Audit": 0},
			"/vms":          {"VM.Audit": 1},
			"/vms/100":      {"VM.Audit": 0, "VM.Config.Disk": 0},
			"/vms/101":      {"VM.Audit": 0, "VM.Config.Disk": 0},
			"/vms/7777":     {"VM.Config.Disk": 0},
			"/storage/pool": {"Datastore.Audit": 0, "Datastore.AllocateSpace": 0, "Datastore.Allocate": 0},
		},
		configs: map[int]map[string]any{
			100: {"digest": "valid-digest", "smbios1": "uuid=153a7c9e-588b-483f-884a-4eba01bc160a", "scsihw": "virtio-scsi-pci", "hotplug": "disk,network"},
			101: {"digest": "valid-digest", "smbios1": "uuid=153a7c9e-588b-483f-884a-4eba01bc160b", "scsihw": "virtio-scsi-single", "hotplug": "1"},
		},
		nodes:     []map[string]string{{"node": "pve"}},
		inventory: []map[string]int{{"vmid": 100}, {"vmid": 101}},
		storage:   map[string]any{"type": "lvmthin", "content": "images", "enabled": 1, "active": 1, "avail": int64(64 << 30)},
		pending:   make(map[int][]map[string]any),
	}
}

func checkTestOptions() volume.CheckOptions {
	return volume.CheckOptions{Node: "pve", Storage: "pool", OwnerVMID: 7777, VMIDs: []int{100, 101}}
}

func newCheckTestService(t *testing.T, client *Client) *volume.CheckService {
	t.Helper()
	gateway, err := NewCheckGateway(client)
	if err != nil {
		t.Fatal(err)
	}
	service, err := volume.NewCheckService(gateway)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func checkTestTarget(target volume.CheckTarget) string {
	switch target.Kind {
	case volume.CheckNodeTarget:
		return "/nodes/" + target.Node
	case volume.CheckInventoryTarget:
		return "/vms"
	case volume.CheckVMTarget:
		return "/vms/" + strconv.Itoa(target.VMID)
	case volume.CheckStorageTarget:
		return "/storage/" + target.Storage
	case volume.CheckPlacementTarget:
		return target.Node + "/" + target.Storage
	case volume.CheckScopeTarget:
		return target.Node
	case volume.CheckConnectionTarget:
		return "HTTPS"
	default:
		return "unknown"
	}
}

func (f *checkFixture) serve(t *testing.T) *Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("check must never mutate PVE: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "PVEAPIToken=test@pve!check="+checkTestSecret {
			t.Error("missing expected test authentication")
		}
		if f.override != nil && f.override(w, r) {
			return
		}
		var data any
		switch r.URL.Path {
		case "/api2/json/nodes":
			data = f.nodes
		case "/api2/json/access/permissions":
			path := r.URL.Query().Get("path")
			grants := f.permissions[path]
			if grants == nil {
				grants = map[string]int{}
			}
			data = map[string]any{path: grants}
		case "/api2/json/nodes/pve/status":
			data = map[string]int{"uptime": 300}
		case "/api2/json/nodes/pve/tasks":
			data = []any{}
		case "/api2/json/cluster/nextid":
			if f.ownerInUse {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"message":"`+checkTestSecret+`"}`)
				return
			}
			data = 7777
		case "/api2/json/nodes/pve/storage/pool/status":
			data = f.storage
		case "/api2/json/nodes/pve/storage/pool/content":
			if r.URL.Query().Get("vmid") != "7777" || r.URL.Query().Get("content") != "images" {
				t.Error("content check must scope to the configured owner and images")
			}
			data = []any{}
		case "/api2/json/nodes/pve/qemu":
			data = f.inventory
		default:
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 8 || parts[5] != "qemu" {
				t.Errorf("unexpected request: %s", r.URL)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			vmid, _ := strconv.Atoi(parts[6])
			switch parts[7] {
			case "config":
				cfg, exists := f.configs[vmid]
				if !exists {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.URL.Query().Get("current") != "1" {
					t.Error("VM config must read the current configuration")
				}
				data = cfg
			case "pending":
				pending := f.pending[vmid]
				if pending == nil {
					pending = []map[string]any{}
				}
				data = pending
			default:
				t.Errorf("unexpected VM read: %s", r.URL)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return &Client{httpClient: server.Client(), config: Config{Endpoint: server.URL, TokenID: "test@pve!check", TokenSecret: checkTestSecret}}
}

func (f *checkFixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestCheckReadOnly(t *testing.T) {
	f := newCheckFixture()
	rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	passes, warnings := 0, 0
	for _, row := range rows {
		switch row.Status {
		case "PASS":
			passes++
		case "WARN":
			warnings++
		default:
			t.Errorf("unexpected unsuccessful check: %+v", row)
		}
		if strings.Contains(fmt.Sprint(row), checkTestSecret) || strings.Contains(fmt.Sprint(row), "test@pve!check") {
			t.Error("check result leaked a credential")
		}
	}
	if passes < 15 || warnings != 2 || f.requestCount() == 0 {
		t.Fatalf("incomplete report: %d passes, %d warnings, %d requests", passes, warnings, f.requestCount())
	}
	for _, path := range []string{"/vms/100", "/vms/101"} {
		assertCheckRow(t, rows, "vm_membership", path, "PASS", "visible VM list")
		assertCheckRow(t, rows, "vm_identity", path, "PASS", "UUID")
		assertCheckRow(t, rows, "vm_attach_readiness", path, "PASS", "free slot scsi1")
	}
}

func TestCheckStoragePermissionsDoNotClaimContentReadSucceeded(t *testing.T) {
	f := newCheckFixture()
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api2/json/nodes/pve/storage/pool/content" {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, checkTestSecret)
		return true
	}
	report, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertCheckRow(t, report, "storage_read_permissions", "/storage/pool", "PASS", "content is checked separately")
	assertCheckRow(t, report, "storage_status", "pve/pool", "PASS", "LVM-thin")
	assertCheckRow(t, report, "storage_content", "pve/pool", "FAIL", "denied")
	if report.Successful() || strings.Contains(fmt.Sprint(report), checkTestSecret) {
		t.Fatal("failed content read passed or exposed the response body")
	}
}

func TestCheckValidatesBeforeRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*volume.CheckOptions)
	}{
		{"invalid node", func(o *volume.CheckOptions) { o.Node = "../pve" }},
		{"invalid storage", func(o *volume.CheckOptions) { o.Storage = "pool\nsecret" }},
		{"invalid owner", func(o *volume.CheckOptions) { o.OwnerVMID = 99 }},
		{"no target", func(o *volume.CheckOptions) { o.VMIDs = nil }},
		{"invalid target", func(o *volume.CheckOptions) { o.VMIDs = []int{-100} }},
		{"duplicate target", func(o *volume.CheckOptions) { o.VMIDs = []int{100, 100} }},
		{"target is owner", func(o *volume.CheckOptions) { o.VMIDs = []int{7777} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckFixture()
			o := checkTestOptions()
			tc.edit(&o)
			rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), o)
			if err == nil || len(rows) != 0 || f.requestCount() != 0 {
				t.Fatalf("invalid options reached PVE: error=%v rows=%d requests=%d", err, len(rows), f.requestCount())
			}
		})
	}
}

func TestCheckOperationalFailures(t *testing.T) {
	for _, tc := range []struct {
		name, row, target, detail string
		edit                      func(*checkFixture)
	}{
		{"missing task permission", "task_permissions", "/nodes/pve", "Sys.Audit", func(f *checkFixture) { delete(f.permissions["/nodes/pve"], "Sys.Audit") }},
		{"missing inventory permission", "inventory_permissions", "/vms", "VM.Audit with propagation", func(f *checkFixture) { delete(f.permissions["/vms"], "VM.Audit") }},
		{"inventory not propagated", "inventory_permissions", "/vms", "VM.Audit with propagation", func(f *checkFixture) { f.permissions["/vms"]["VM.Audit"] = 0 }},
		{"missing creation permission", "storage_create_permission", "/storage/pool", "Datastore.AllocateSpace", func(f *checkFixture) { delete(f.permissions["/storage/pool"], "Datastore.AllocateSpace") }},
		{"missing deletion permission", "storage_delete_permission", "/storage/pool", "Datastore.Allocate", func(f *checkFixture) { delete(f.permissions["/storage/pool"], "Datastore.Allocate") }},
		{"missing storage read permission", "storage_read_permissions", "/storage/pool", "Datastore.Audit", func(f *checkFixture) { f.permissions["/storage/pool"] = map[string]int{} }},
		{"missing VM read permission", "vm_read_permission", "/vms/100", "VM.Audit", func(f *checkFixture) { delete(f.permissions["/vms/100"], "VM.Audit") }},
		{"missing VM disk permission", "vm_disk_permission", "/vms/100", "VM.Config.Disk", func(f *checkFixture) { delete(f.permissions["/vms/100"], "VM.Config.Disk") }},
		{"absent explicit VM", "vm_membership", "/vms/100", "not found in visible VM list", func(f *checkFixture) { delete(f.configs, 100); f.inventory = []map[string]int{{"vmid": 101}} }},
		{"missing selected UUID", "vm_identity", "/vms/100", "no SMBIOS UUID", func(f *checkFixture) { delete(f.configs[100], "smbios1") }},
		{"malformed selected UUID", "vm_identity", "/vms/100", "UUIDs", func(f *checkFixture) { f.configs[100]["smbios1"] = "uuid=invalid" }},
		{"duplicate UUID", "vm_uuid_unique", "/vms/100", "multiple visible VMs", func(f *checkFixture) { f.configs[101]["smbios1"] = f.configs[100]["smbios1"] }},
		{"selected VM hidden from list", "vm_membership", "/vms/100", "verify VMID/node or permissions", func(f *checkFixture) { f.inventory = []map[string]int{{"vmid": 101}} }},
		{"template selected", "vm_identity", "/vms/100", "non-template", func(f *checkFixture) { f.configs[100]["template"] = 1; f.inventory[0]["template"] = 1 }},
		{"storage type", "storage_status", "pve/pool", "LVM-thin", func(f *checkFixture) { f.storage["type"] = "dir" }},
		{"storage content", "storage_status", "pve/pool", "support images", func(f *checkFixture) { f.storage["content"] = "iso" }},
		{"storage disabled", "storage_status", "pve/pool", "disabled", func(f *checkFixture) { f.storage["enabled"] = 0 }},
		{"disabled unsupported storage", "storage_status", "pve/pool", "disabled", func(f *checkFixture) { f.storage["enabled"] = 0; f.storage["type"] = "dir" }},
		{"storage inactive", "storage_status", "pve/pool", "inactive", func(f *checkFixture) { f.storage["active"] = 0 }},
		{"inactive unsupported storage", "storage_status", "pve/pool", "inactive", func(f *checkFixture) { f.storage["active"] = 0; f.storage["type"] = "dir" }},
		{"storage full", "storage_status", "pve/pool", "less than 1 GiB", func(f *checkFixture) { f.storage["avail"] = 0 }},
		{"storage malformed", "storage_status", "pve/pool", "invalid or unexpected response", func(f *checkFixture) { delete(f.storage, "avail") }},
		{"owner occupied", "reserved_owner", "/vms/7777", "used by a VM or container", func(f *checkFixture) { f.ownerInUse = true }},
		{"pending VM changes", "vm_attach_readiness", "/vms/100", "pending changes", func(f *checkFixture) { f.pending[100] = []map[string]any{{"key": "memory", "pending": 8192}} }},
		{"locked VM", "vm_attach_readiness", "/vms/100", "locked", func(f *checkFixture) { f.configs[100]["lock"] = "backup" }},
		{"disk hotplug disabled", "vm_attach_readiness", "/vms/100", "enable disk hotplug", func(f *checkFixture) { f.configs[100]["hotplug"] = "network" }},
		{"wrong SCSI hardware", "vm_attach_readiness", "/vms/100", "VirtIO SCSI", func(f *checkFixture) { f.configs[100]["scsihw"] = "lsi" }},
		{"no free disk slots", "vm_attach_readiness", "/vms/100", "no free SCSI", func(f *checkFixture) {
			for i := 1; i <= 30; i++ {
				f.configs[100][fmt.Sprintf("scsi%d", i)] = "pool:existing-disk"
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckFixture()
			tc.edit(f)
			rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
			if err != nil {
				t.Fatalf("operational failures must be report rows: %v", err)
			}
			assertCheckRow(t, rows, tc.row, tc.target, "FAIL", tc.detail)
			if strings.Contains(fmt.Sprint(rows), checkTestSecret) {
				t.Fatal("server text or credential leaked")
			}
		})
	}
}

func TestCheckMinimumVolumeCapacity(t *testing.T) {
	for _, available := range []int64{0, 1, domain.GiB - 1, domain.GiB} {
		t.Run(strconv.FormatInt(available, 10), func(t *testing.T) {
			f := newCheckFixture()
			f.storage["avail"] = available
			rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			status := "FAIL"
			if available >= domain.GiB {
				status = "PASS"
			}
			assertCheckRow(t, rows, "storage_status", "pve/pool", status, "")
		})
	}
}

func TestCheckOptionsVolumeHandleLength(t *testing.T) {
	for _, tc := range []struct {
		nodeLength int
		valid      bool
	}{
		{39, true}, // 128-byte volume handle with this storage and owner.
		{40, false},
	} {
		options := checkTestOptions()
		options.Node = strings.Repeat("n", tc.nodeLength)
		options.Storage = "data-ssd"
		if err := options.Validate(); (err == nil) != tc.valid {
			t.Errorf("node length %d: error=%v, want valid=%t", tc.nodeLength, err, tc.valid)
		}
	}
}

func TestCheckReportsIndependentPermissionFailures(t *testing.T) {
	f := newCheckFixture()
	f.permissions["/nodes/pve"] = map[string]int{}
	f.permissions["/vms"] = map[string]int{}
	f.permissions["/storage/pool"] = map[string]int{}
	f.permissions["/vms/100"] = map[string]int{}
	rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ name, target, detail string }{
		{"task_permissions", "/nodes/pve", "Sys.Audit"},
		{"inventory_permissions", "/vms", "VM.Audit with propagation"},
		{"storage_create_permission", "/storage/pool", "Datastore.AllocateSpace"},
		{"storage_delete_permission", "/storage/pool", "Datastore.Allocate"},
		{"vm_read_permission", "/vms/100", "VM.Audit"},
		{"vm_disk_permission", "/vms/100", "VM.Config.Disk"},
	} {
		assertCheckRow(t, rows, want.name, want.target, "FAIL", want.detail)
	}
}

func TestCheckMissingNodeSkipsNodeScopedRequests(t *testing.T) {
	for _, node := range []string{"typo", "pve"} {
		t.Run(node, func(t *testing.T) {
			f := newCheckFixture()
			if node == "pve" {
				f.nodes = []map[string]string{}
			}
			// PVE may return HTTP 500 for a missing node even when its ACL path exists.
			f.permissions["/nodes/"+node] = map[string]int{"Sys.Audit": 0}
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasPrefix(r.URL.Path, "/api2/json/nodes/"+node+"/") {
					t.Errorf("missing node detail requested: %s", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
					io.WriteString(w, checkTestSecret)
					return true
				}
				return false
			}
			options := checkTestOptions()
			options.Node = node
			rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			assertCheckRow(t, rows, "node_membership", "/nodes/"+node, "FAIL", "not found in visible node list; verify node name or permissions")
			assertCheckRow(t, rows, "node_status", "/nodes/"+node, "SKIP", "not verified")
			assertCheckRow(t, rows, "reserved_owner", "/vms/7777", "PASS", "unused")
			assertCheckRow(t, rows, "storage_create_permission", "/storage/pool", "PASS", "Datastore.AllocateSpace")
			assertCheckRow(t, rows, "vm_identity", "/vms/100", "SKIP", "membership")
			if strings.Contains(fmt.Sprint(rows), "HTTP 500") || strings.Contains(fmt.Sprint(rows), "unavailable") || strings.Contains(fmt.Sprint(rows), checkTestSecret) {
				t.Fatal("absent visible node was reported as a service failure or leaked server text")
			}
		})
	}
}

func TestCheckMissingVMDoesNotBlockOtherSelectedVM(t *testing.T) {
	f := newCheckFixture()
	f.inventory = []map[string]int{{"vmid": 101}}
	delete(f.configs, 100)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve/qemu/100/") {
			t.Errorf("missing VM detail requested: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, checkTestSecret)
			return true
		}
		return false
	}
	rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertCheckRow(t, rows, "vm_membership", "/vms/100", "FAIL", "not found in visible VM list")
	assertCheckRow(t, rows, "vm_read_permission", "/vms/100", "PASS", "VM.Audit")
	assertCheckRow(t, rows, "vm_identity", "/vms/100", "SKIP", "membership")
	assertCheckRow(t, rows, "vm_attach_readiness", "/vms/100", "SKIP", "not both verified")
	assertCheckRow(t, rows, "vm_identity", "/vms/101", "PASS", "UUID")
	assertCheckRow(t, rows, "vm_uuid_unique", "/vms/101", "PASS", "uniquely")
	assertCheckRow(t, rows, "vm_attach_readiness", "/vms/101", "PASS", "free slot")
	if strings.Contains(fmt.Sprint(rows), "HTTP 500") || strings.Contains(fmt.Sprint(rows), "unavailable") {
		t.Fatal("absent VM was incorrectly reported as a service failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	listReads := 0
	for _, request := range f.requests {
		if request == "GET /api2/json/nodes/pve/qemu" {
			listReads++
		}
	}
	if listReads != 1 {
		t.Fatalf("VM inventory should reuse the membership list; got %d list requests", listReads)
	}
}

func TestCheckServerErrorAfterMembershipRemainsFatal(t *testing.T) {
	for _, tc := range []struct{ path, row, target string }{
		{"/api2/json/nodes", "node_membership", "/nodes/pve"},
		{"/api2/json/nodes/pve/status", "node_status", "/nodes/pve"},
		{"/api2/json/nodes/pve/qemu", "vm_list", "/nodes/pve"},
		// A VM may disappear after a successful list. Its unknown 500 must
		// not be reclassified as definitively absent by parsing server text.
		{"/api2/json/nodes/pve/qemu/100/config", "vm_inventory", "/nodes/pve"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			f := newCheckFixture()
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != tc.path {
					return false
				}
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "Configuration does not exist "+checkTestSecret)
				return true
			}
			rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			assertCheckRow(t, rows, tc.row, tc.target, "FAIL", "PVE returned HTTP 500")
			failed := false
			for _, row := range rows {
				if row.Name == tc.row && checkTestTarget(row.Target) == tc.target {
					failed = true
					continue
				}
				if failed && row.Status != "SKIP" && row.Status != "WARN" {
					t.Fatalf("continued after an unknown server failure: %+v", row)
				}
			}
			if strings.Contains(fmt.Sprint(rows), checkTestSecret) || strings.Contains(fmt.Sprint(rows), "network connectivity") {
				t.Fatal("server error leaked details or was blamed on network connectivity")
			}
		})
	}
}

func TestCheckStopsAfterAuthenticationOrServiceFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newCheckFixture()
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				w.WriteHeader(status)
				io.WriteString(w, checkTestSecret)
				return true
			}
			rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
			if err != nil || f.requestCount() != 1 || rows[0].Status != "FAIL" {
				t.Fatalf("failed to stop: error=%v requests=%d rows=%+v", err, f.requestCount(), rows)
			}
			for _, row := range rows[1:] {
				if row.Status != "SKIP" && row.Status != "WARN" {
					t.Fatalf("ran a check after fatal failure: %+v", row)
				}
			}
			if strings.Contains(fmt.Sprint(rows), checkTestSecret) {
				t.Fatal("response body leaked")
			}
		})
	}
}

func TestCheckContextDeadlineAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			f := newCheckFixture()
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				<-r.Context().Done()
				return true
			}
			client := f.serve(t)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if canceled {
				cancel()
			}
			started := time.Now()
			rows, err := newCheckTestService(t, client).Check(ctx, checkTestOptions())
			if err != nil || time.Since(started) > time.Second || f.requestCount() > 1 {
				t.Fatalf("unbounded check: error=%v duration=%v requests=%d", err, time.Since(started), f.requestCount())
			}
			detail := "timed out"
			if canceled {
				detail = "canceled"
				if f.requestCount() != 0 {
					t.Error("canceled check made a request")
				}
			}
			assertCheckRow(t, rows, "task_permissions", "/nodes/pve", "FAIL", detail)
		})
	}
}

func TestCheckMalformedResponseIsSafe(t *testing.T) {
	f := newCheckFixture()
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api2/json/nodes/pve/storage/pool/status" {
			return false
		}
		io.WriteString(w, checkTestSecret)
		return true
	}
	rows, err := newCheckTestService(t, f.serve(t)).Check(context.Background(), checkTestOptions())
	if err != nil || strings.Contains(fmt.Sprint(rows), checkTestSecret) {
		t.Fatalf("unsafe malformed response: error=%v rows=%+v", err, rows)
	}
	assertCheckRow(t, rows, "storage_status", "pve/pool", "FAIL", "invalid or unexpected response")
	assertCheckRow(t, rows, "vm_attach_readiness", "/vms/100", "PASS", "free slot")
}

func assertCheckRow(t *testing.T, rows volume.CheckReport, name, target, status, detail string) {
	t.Helper()
	for _, row := range rows {
		if row.Name == name && checkTestTarget(row.Target) == target {
			if row.Status != status || !strings.Contains(row.Detail, detail) {
				t.Fatalf("wrong %s on %s: got %+v; want %s containing %q", name, target, row, status, detail)
			}
			return
		}
	}
	t.Fatalf("missing row %s on %s", name, target)
}
