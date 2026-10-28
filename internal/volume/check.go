package volume

import (
	"context"
	"errors"

	"pve-csi.local/csi-plugin/internal/domain"
)

// CheckOptions requires explicit VMIDs so filtered inventories cannot pass silently.
type CheckOptions struct {
	Node      string
	Storage   string
	OwnerVMID int
	VMIDs     []int
}

func (o CheckOptions) Validate() error {
	if !domain.ValidNodeName(o.Node) {
		return errors.New("node must be a valid host name")
	}
	if !domain.ValidStorageID(o.Storage) {
		return errors.New("storage must be a valid storage ID")
	}
	if !domain.ValidVMID(o.OwnerVMID) {
		return errors.New("owner VMID must be an integer from 100 to 999999999")
	}
	if _, err := domain.NewRequest("check", domain.Placement{Node: o.Node, Storage: o.Storage}, o.OwnerVMID, domain.GiB, 0); err != nil {
		return errors.New("node, storage and owner VMID must produce a volume handle within 128 bytes")
	}
	if len(o.VMIDs) == 0 {
		return errors.New("at least one explicit Kubernetes VMID is required")
	}
	seen := make(map[int]bool, len(o.VMIDs))
	for _, id := range o.VMIDs {
		if !domain.ValidVMID(id) {
			return errors.New("each Kubernetes VMID must be an integer from 100 to 999999999")
		}
		if id == o.OwnerVMID {
			return errors.New("selected Kubernetes VMIDs must differ from the reserved owner VMID")
		}
		if seen[id] {
			return errors.New("selected Kubernetes VMIDs must be unique")
		}
		seen[id] = true
	}
	return nil
}

type CheckTargetKind string

const (
	CheckNodeTarget       CheckTargetKind = "node"
	CheckInventoryTarget  CheckTargetKind = "inventory"
	CheckVMTarget         CheckTargetKind = "vm"
	CheckStorageTarget    CheckTargetKind = "storage"
	CheckPlacementTarget  CheckTargetKind = "placement"
	CheckScopeTarget      CheckTargetKind = "scope"
	CheckConnectionTarget CheckTargetKind = "connection"
)

// CheckTarget identifies a resource without encoding its display path.
type CheckTarget struct {
	Kind    CheckTargetKind
	Node    string
	Storage string
	VMID    int
}

const (
	CheckPass = "PASS"
	CheckFail = "FAIL"
	CheckWarn = "WARN"
	CheckSkip = "SKIP"
)

// CheckResult contains authored diagnostics, not authorization for later writes.
type CheckResult struct {
	Name   string
	Target CheckTarget
	Status string
	Detail string
}

type CheckReport []CheckResult

func (report CheckReport) Successful() bool {
	passed := false
	for _, result := range report {
		switch result.Status {
		case CheckPass:
			passed = true
		case CheckWarn:
		default:
			return false
		}
	}
	return passed
}

// CheckVM includes templates for inventory checks, but excludes them as node identities.
type CheckVM struct {
	VMID     int
	Template bool
}

type CheckPoolObservation struct {
	Pool              domain.Pool
	Detail            string // Displayed after domain checks pass.
	UnsupportedDetail string // Format requirements when SupportsVolumes is false.
}

// CheckGateway permits read-only observations. Adapters must return authored
// diagnostics, never response bodies or credentials.
type CheckGateway interface {
	TaskAccess(context.Context, string) (string, error)
	InventoryAccess(context.Context) (string, error)
	VisibleNodes(context.Context) ([]string, error)
	NodeStatus(context.Context, string) (string, error)
	ActiveTasks(context.Context, string) (string, error)
	OwnerUnused(context.Context, int) (bool, error)
	StorageReadAccess(context.Context, domain.Placement, int) (string, error)
	StorageAllocationAccess(context.Context, domain.Placement) (string, error)
	StorageDeletionAccess(context.Context, domain.Placement) (string, error)
	InspectPool(context.Context, domain.Placement, int) (CheckPoolObservation, error)
	ImageCount(context.Context, domain.Placement, int) (int, error)
	ListVMs(context.Context, string) ([]CheckVM, error)
	VMIdentities(context.Context, string, []CheckVM) ([]domain.VirtualMachine, error)
	VMReadAccess(context.Context, int) (string, error)
	VMDiskAccess(context.Context, int) (string, error)
	VMIdentity(context.Context, string, int) (domain.VirtualMachine, error)
	AttachmentReadiness(context.Context, domain.VirtualMachine) (string, error)
}

// CheckFailure marks an observation failure. Terminal failures stop the check
// because further observations would be unreliable, such as after connection loss.
type CheckFailure struct {
	hint     string
	terminal bool
	cause    error
}

func NewCheckFailure(hint string, terminal bool, cause error) *CheckFailure {
	return &CheckFailure{hint: hint, terminal: terminal, cause: cause}
}

func (e *CheckFailure) Error() string  { return "check observation failed" }
func (e *CheckFailure) Hint() string   { return e.hint }
func (e *CheckFailure) Terminal() bool { return e.terminal }
func (e *CheckFailure) Unwrap() error  { return e.cause }
