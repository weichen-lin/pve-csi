package volume

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
)

// This port has no write operations or transport. Calls describe observations
// so tests can assert what was skipped, even when a report has useful failures.
type checkGatewayStub struct {
	calls       []string
	failures    map[string]error
	after       func(string)
	nodes       []string
	entries     []CheckVM
	machines    []domain.VirtualMachine
	identities  map[int]domain.VirtualMachine
	pool        CheckPoolObservation
	ownerUnused bool
	listedInput []CheckVM
}

func (g *checkGatewayStub) observe(name string) error {
	g.calls = append(g.calls, name)
	if g.after != nil {
		g.after(name)
	}
	return g.failures[name]
}
func (g *checkGatewayStub) TaskAccess(context.Context, string) (string, error) {
	return "task capability observed", g.observe("task")
}
func (g *checkGatewayStub) InventoryAccess(context.Context) (string, error) {
	return "inventory capability observed", g.observe("inventory-access")
}
func (g *checkGatewayStub) VisibleNodes(context.Context) ([]string, error) {
	return g.nodes, g.observe("nodes")
}
func (g *checkGatewayStub) NodeStatus(context.Context, string) (string, error) {
	return "node status observed", g.observe("node-status")
}
func (g *checkGatewayStub) ActiveTasks(context.Context, string) (string, error) {
	return "task list observed", g.observe("tasks")
}
func (g *checkGatewayStub) OwnerUnused(context.Context, int) (bool, error) {
	return g.ownerUnused, g.observe("owner")
}
func (g *checkGatewayStub) StorageReadAccess(context.Context, domain.Placement, int) (string, error) {
	return "storage read capability observed", g.observe("storage-read")
}
func (g *checkGatewayStub) StorageAllocationAccess(context.Context, domain.Placement) (string, error) {
	return "storage allocation capability observed", g.observe("storage-allocation")
}
func (g *checkGatewayStub) StorageDeletionAccess(context.Context, domain.Placement) (string, error) {
	return "storage deletion capability observed", g.observe("storage-deletion")
}
func (g *checkGatewayStub) InspectPool(context.Context, domain.Placement, int) (CheckPoolObservation, error) {
	return g.pool, g.observe("pool")
}
func (g *checkGatewayStub) ImageCount(context.Context, domain.Placement, int) (int, error) {
	return 2, g.observe("images")
}
func (g *checkGatewayStub) ListVMs(context.Context, string) ([]CheckVM, error) {
	return g.entries, g.observe("vms")
}
func (g *checkGatewayStub) VMIdentities(_ context.Context, _ string, entries []CheckVM) ([]domain.VirtualMachine, error) {
	g.listedInput = slices.Clone(entries)
	return g.machines, g.observe("identities")
}
func (g *checkGatewayStub) VMReadAccess(_ context.Context, id int) (string, error) {
	return "VM read capability observed", g.observe(fmt.Sprintf("read:%d", id))
}
func (g *checkGatewayStub) VMDiskAccess(_ context.Context, id int) (string, error) {
	return "VM disk capability observed", g.observe(fmt.Sprintf("disk:%d", id))
}
func (g *checkGatewayStub) VMIdentity(_ context.Context, _ string, id int) (domain.VirtualMachine, error) {
	return g.identities[id], g.observe(fmt.Sprintf("identity:%d", id))
}
func (g *checkGatewayStub) AttachmentReadiness(_ context.Context, vm domain.VirtualMachine) (string, error) {
	return "attachment readiness observed", g.observe(fmt.Sprintf("ready:%d", vm.VMID))
}

func checkServiceFixture(t *testing.T) (*CheckService, *checkGatewayStub, CheckOptions) {
	t.Helper()
	id100, err := domain.ParseNodeID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	id101, err := domain.ParseNodeID("22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	machines := []domain.VirtualMachine{
		{Node: "node-a", VMID: 100, ID: id100},
		{Node: "node-a", VMID: 101, ID: id101},
	}
	gateway := &checkGatewayStub{
		failures: make(map[string]error), nodes: []string{"node-a"},
		entries: []CheckVM{{VMID: 100}, {VMID: 101}}, machines: machines,
		identities: map[int]domain.VirtualMachine{100: machines[0], 101: machines[1]},
		pool: CheckPoolObservation{
			Pool:   domain.Pool{Enabled: true, Active: true, SupportsVolumes: true, AvailableBytes: 10 * domain.GiB},
			Detail: "supported storage with observed free capacity",
		},
		ownerUnused: true,
	}
	service, err := NewCheckService(gateway)
	if err != nil {
		t.Fatal(err)
	}
	return service, gateway, CheckOptions{Node: "node-a", Storage: "pool", OwnerVMID: 7777, VMIDs: []int{100, 101}}
}

func runServiceCheck(t *testing.T, service *CheckService, ctx context.Context, options CheckOptions) CheckReport {
	t.Helper()
	report, err := service.Check(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func requireCheckRow(t *testing.T, report CheckReport, name string, vmid int, status string) CheckResult {
	t.Helper()
	for _, result := range report {
		if result.Name == name && result.Target.VMID == vmid {
			if result.Status != status {
				t.Fatalf("%s VMID %d: got %s (%s), want %s", name, vmid, result.Status, result.Detail, status)
			}
			return result
		}
	}
	t.Fatalf("missing %s VMID %d", name, vmid)
	return CheckResult{}
}

func TestCheckServiceSuccess(t *testing.T) {
	service, gateway, options := checkServiceFixture(t)
	report := runServiceCheck(t, service, context.Background(), options)
	if !report.Successful() {
		t.Fatalf("expected successful report: %+v", report)
	}
	wantNames := []string{
		"task_permissions", "inventory_permissions", "node_membership", "node_status", "task_api",
		"reserved_owner", "storage_read_permissions", "storage_create_permission", "storage_delete_permission",
		"storage_status", "storage_content", "vm_list", "vm_inventory",
		"vm_membership", "vm_read_permission", "vm_disk_permission", "vm_identity", "vm_uuid_unique", "vm_attach_readiness",
		"vm_membership", "vm_read_permission", "vm_disk_permission", "vm_identity", "vm_uuid_unique", "vm_attach_readiness",
		"inventory_scope", "readonly_scope",
	}
	var names []string
	for i, result := range report {
		names = append(names, result.Name)
		wantStatus := CheckPass
		if i >= len(wantNames)-2 {
			wantStatus = CheckWarn
		}
		if result.Status != wantStatus || result.Detail == "" || result.Target.Kind == "" {
			t.Fatalf("incomplete result: %+v", result)
		}
	}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("result ordering: %v", names)
	}
	if !reflect.DeepEqual(gateway.listedInput, gateway.entries) {
		t.Fatal("identity inventory must use the observed membership list")
	}
	if report[0].Target != (CheckTarget{Kind: CheckNodeTarget, Node: "node-a"}) ||
		report[1].Target != (CheckTarget{Kind: CheckInventoryTarget}) ||
		report[5].Target != (CheckTarget{Kind: CheckVMTarget, VMID: 7777}) ||
		report[6].Target != (CheckTarget{Kind: CheckStorageTarget, Storage: "pool"}) ||
		report[9].Target != (CheckTarget{Kind: CheckPlacementTarget, Node: "node-a", Storage: "pool"}) ||
		report[len(report)-1].Target != (CheckTarget{Kind: CheckScopeTarget, Node: "node-a"}) {
		t.Fatal("results must identify typed application resources")
	}
	if report[9].Detail != gateway.pool.Detail {
		t.Fatal("successful storage observation lost its adapter-authored detail")
	}
}

func TestCheckServiceRejectsInvalidInputsBeforeObservations(t *testing.T) {
	if _, err := NewCheckService(nil); err == nil {
		t.Fatal("nil gateway accepted")
	}
	for _, change := range []func(*CheckOptions){
		func(o *CheckOptions) { o.Node = "../node" },
		func(o *CheckOptions) { o.Storage = "../pool" },
		func(o *CheckOptions) { o.OwnerVMID = 99 },
		func(o *CheckOptions) { o.VMIDs = nil },
		func(o *CheckOptions) { o.VMIDs = []int{99} },
		func(o *CheckOptions) { o.VMIDs = []int{7777} },
		func(o *CheckOptions) { o.VMIDs = []int{100, 100} },
		func(o *CheckOptions) { o.Node = strings.Repeat("n", 50) },
	} {
		service, gateway, options := checkServiceFixture(t)
		change(&options)
		if report, err := service.Check(context.Background(), options); err == nil || len(report) != 0 || len(gateway.calls) != 0 {
			t.Fatalf("invalid options reached observations: error=%v calls=%v", err, gateway.calls)
		}
	}
	service, gateway, options := checkServiceFixture(t)
	if _, err := service.Check(nil, options); err == nil || len(gateway.calls) != 0 {
		t.Fatal("nil context must be rejected before observations")
	}
	var uninitialized *CheckService
	if _, err := uninitialized.Check(context.Background(), options); err == nil {
		t.Fatal("nil service must be rejected")
	}
}

func TestCheckServiceMembershipGatesDetails(t *testing.T) {
	t.Run("absent node", func(t *testing.T) {
		service, gateway, options := checkServiceFixture(t)
		gateway.nodes = []string{"other-node"}
		report := runServiceCheck(t, service, context.Background(), options)
		result := requireCheckRow(t, report, "node_membership", 0, CheckFail)
		if !strings.Contains(result.Detail, "node name or permissions") {
			t.Fatalf("missing actionable node hint: %s", result.Detail)
		}
		for _, call := range []string{"node-status", "tasks", "pool", "images", "vms", "identities", "identity:100", "ready:100", "identity:101", "ready:101"} {
			if slices.Contains(gateway.calls, call) {
				t.Errorf("absent node triggered scoped observation %s", call)
			}
		}
		requireCheckRow(t, report, "reserved_owner", 7777, CheckPass)
		requireCheckRow(t, report, "storage_delete_permission", 0, CheckPass)
		requireCheckRow(t, report, "vm_identity", 100, CheckSkip)
	})
	t.Run("absent VM does not stop the other VM", func(t *testing.T) {
		service, gateway, options := checkServiceFixture(t)
		gateway.entries = []CheckVM{{VMID: 101}}
		gateway.machines = gateway.machines[1:]
		report := runServiceCheck(t, service, context.Background(), options)
		requireCheckRow(t, report, "vm_membership", 100, CheckFail)
		requireCheckRow(t, report, "vm_identity", 100, CheckSkip)
		requireCheckRow(t, report, "vm_uuid_unique", 101, CheckPass)
		requireCheckRow(t, report, "vm_attach_readiness", 101, CheckPass)
		if slices.Contains(gateway.calls, "identity:100") || slices.Contains(gateway.calls, "ready:100") {
			t.Fatal("absent VM triggered detail observations")
		}
	})
}

func TestCheckServiceIndependentFailuresAndPrerequisites(t *testing.T) {
	service, gateway, options := checkServiceFixture(t)
	gateway.failures["storage-read"] = NewCheckFailure("storage read denied", false, errors.New("private response"))
	gateway.failures["disk:100"] = NewCheckFailure("disk changes denied", false, nil)
	gateway.failures["inventory-access"] = NewCheckFailure("inventory access denied", false, nil)
	report := runServiceCheck(t, service, context.Background(), options)
	requireCheckRow(t, report, "storage_read_permissions", 0, CheckFail)
	requireCheckRow(t, report, "storage_content", 0, CheckSkip)
	requireCheckRow(t, report, "storage_status", 0, CheckPass)
	requireCheckRow(t, report, "vm_inventory", 0, CheckSkip)
	requireCheckRow(t, report, "vm_uuid_unique", 101, CheckSkip)
	requireCheckRow(t, report, "vm_attach_readiness", 100, CheckSkip)
	requireCheckRow(t, report, "vm_attach_readiness", 101, CheckPass)
	if slices.Contains(gateway.calls, "images") || slices.Contains(gateway.calls, "identities") || slices.Contains(gateway.calls, "ready:100") {
		t.Fatal("failed prerequisites triggered dependent observations")
	}
	if report.Successful() || strings.Contains(fmt.Sprint(report), "private response") {
		t.Fatal("failed report passed or exposed an underlying error")
	}
}

func TestCheckServiceTerminalFailures(t *testing.T) {
	for _, failure := range []error{
		NewCheckFailure("connection unavailable", true, errors.New("private response")),
		NewCheckFailure("safe hint that must not hide cancellation", false, context.Canceled),
		NewCheckFailure("safe hint that must not hide deadline", false, context.DeadlineExceeded),
	} {
		service, gateway, options := checkServiceFixture(t)
		gateway.failures["nodes"] = failure
		report := runServiceCheck(t, service, context.Background(), options)
		requireCheckRow(t, report, "node_membership", 0, CheckFail)
		if !slices.Equal(gateway.calls, []string{"task", "inventory-access", "nodes"}) {
			t.Fatalf("terminal failure allowed subsequent observations: %v", gateway.calls)
		}
		for _, result := range report[3 : len(report)-2] {
			if result.Status != CheckSkip {
				t.Fatalf("result after terminal failure was not skipped: %+v", result)
			}
		}
		if strings.Contains(fmt.Sprint(report), "private response") || strings.Contains(fmt.Sprint(report), "safe hint that must not") {
			t.Fatal("failure precedence or redaction was violated")
		}
	}
}

func TestCheckServiceContextStopsObservations(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		service, gateway, options := checkServiceFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		} else {
			cancel()
		}
		report := runServiceCheck(t, service, ctx, options)
		cancel()
		requireCheckRow(t, report, "task_permissions", 0, CheckFail)
		if len(gateway.calls) != 0 || report.Successful() {
			t.Fatal("completed context must stop all observations")
		}
	}
	t.Run("canceled during an observation", func(t *testing.T) {
		service, gateway, options := checkServiceFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		gateway.after = func(string) { cancel() }
		gateway.failures["task"] = NewCheckFailure("permission denied", false, nil)
		report := runServiceCheck(t, service, ctx, options)
		result := requireCheckRow(t, report, "task_permissions", 0, CheckFail)
		if len(gateway.calls) != 1 || result.Detail != "check canceled" {
			t.Fatalf("cancellation must take precedence: calls=%v result=%+v", gateway.calls, result)
		}
	})
}

func TestCheckServiceDomainRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*checkGatewayStub)
		row    string
		vmid   int
		hint   string
	}{
		{"owner collision", func(g *checkGatewayStub) { g.ownerUnused = false }, "reserved_owner", 7777, "owner VMID is used"},
		{"minimum capacity", func(g *checkGatewayStub) { g.pool.Pool.AvailableBytes = domain.GiB - 1 }, "storage_status", 0, "less than 1 GiB"},
		{"disabled storage", func(g *checkGatewayStub) { g.pool.Pool.Enabled = false }, "storage_status", 0, "disabled"},
		{"inactive storage", func(g *checkGatewayStub) { g.pool.Pool.Active = false }, "storage_status", 0, "inactive"},
		{"unsupported storage", func(g *checkGatewayStub) { g.pool.Pool.SupportsVolumes = false }, "storage_status", 0, "does not support"},
		{"duplicate UUID", func(g *checkGatewayStub) { g.machines[1].ID = g.machines[0].ID }, "vm_uuid_unique", 100, "multiple visible VMs"},
		{"UUID absent", func(g *checkGatewayStub) { g.machines = g.machines[1:] }, "vm_uuid_unique", 100, "absent"},
		{"wrong inventory VM", func(g *checkGatewayStub) { g.machines[0].VMID = 102 }, "vm_uuid_unique", 100, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, gateway, options := checkServiceFixture(t)
			tc.change(gateway)
			report := runServiceCheck(t, service, context.Background(), options)
			result := requireCheckRow(t, report, tc.row, tc.vmid, CheckFail)
			if !strings.Contains(result.Detail, tc.hint) || report.Successful() {
				t.Fatalf("wrong domain outcome: %+v", result)
			}
			if !slices.Contains(gateway.calls, "ready:101") {
				t.Fatal("ordinary domain failure incorrectly stopped independent observations")
			}
		})
	}
	t.Run("exactly minimum capacity", func(t *testing.T) {
		service, gateway, options := checkServiceFixture(t)
		gateway.pool.Pool.AvailableBytes = domain.GiB
		report := runServiceCheck(t, service, context.Background(), options)
		requireCheckRow(t, report, "storage_status", 0, CheckPass)
	})
}

func TestCheckServiceFailureHints(t *testing.T) {
	for _, tc := range []struct {
		failure error
		want    string
	}{
		{NewCheckFailure("adapter-specific supported format hint", false, domain.ErrStorage), "adapter-specific supported format hint"},
		{domain.ErrStorage, "storage does not support CSI volumes"},
		{errors.New("private response or credential"), "check failed with an unexpected response"},
	} {
		service, gateway, options := checkServiceFixture(t)
		gateway.failures["pool"] = tc.failure
		report := runServiceCheck(t, service, context.Background(), options)
		result := requireCheckRow(t, report, "storage_status", 0, CheckFail)
		if result.Detail != tc.want {
			t.Fatalf("safe hint=%q, want %q", result.Detail, tc.want)
		}
	}
}

func TestCheckServiceStorageReadinessPrecedesFormatHint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		active  bool
		support bool
		status  string
		want    string
	}{
		{"disabled", false, false, false, CheckFail, "storage is disabled"},
		{"inactive", true, false, false, CheckFail, "storage is inactive on the configured node"},
		{"unsupported", true, true, false, CheckFail, "storage requires the adapter's supported image format"},
		{"supported", true, true, true, CheckPass, "supported storage with observed free capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, gateway, options := checkServiceFixture(t)
			gateway.pool.Pool.Enabled = tc.enabled
			gateway.pool.Pool.Active = tc.active
			gateway.pool.Pool.SupportsVolumes = tc.support
			gateway.pool.UnsupportedDetail = "storage requires the adapter's supported image format"
			report := runServiceCheck(t, service, context.Background(), options)
			result := requireCheckRow(t, report, "storage_status", 0, tc.status)
			if result.Detail != tc.want {
				t.Fatalf("readiness precedence: got %q, want %q", result.Detail, tc.want)
			}
		})
	}
}

func TestCheckReportPolicy(t *testing.T) {
	for _, tc := range []struct {
		statuses []string
		success  bool
	}{
		{[]string{CheckPass, CheckWarn}, true},
		{[]string{CheckPass, CheckFail}, false},
		{[]string{CheckPass, CheckSkip}, false},
		{[]string{CheckWarn}, false},
		{[]string{"UNKNOWN"}, false},
		{nil, false},
	} {
		var report CheckReport
		for _, status := range tc.statuses {
			report = append(report, CheckResult{Status: status})
		}
		if report.Successful() != tc.success {
			t.Fatalf("report %v success=%t, want %t", tc.statuses, report.Successful(), tc.success)
		}
	}
}
