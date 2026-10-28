package volume

import (
	"context"
	"errors"
	"fmt"

	"pve-csi.local/csi-plugin/internal/domain"
)

// CheckService runs read-only checks in prerequisite order.
type CheckService struct {
	gateway CheckGateway
}

func NewCheckService(gateway CheckGateway) (*CheckService, error) {
	if gateway == nil {
		return nil, errors.New("check service requires a read-only gateway")
	}
	return &CheckService{gateway: gateway}, nil
}

// Check reports operational failures as rows and invalid inputs as errors.
// Callers should set a deadline for the entire check.
func (s *CheckService) Check(ctx context.Context, options CheckOptions) (CheckReport, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.gateway == nil {
		return nil, errors.New("check requires an initialized service")
	}
	gateway := s.gateway
	run := checkRun{ctx: ctx}
	nodeTarget := CheckTarget{Kind: CheckNodeTarget, Node: options.Node}
	inventoryTarget := CheckTarget{Kind: CheckInventoryTarget}
	storageTarget := CheckTarget{Kind: CheckStorageTarget, Storage: options.Storage}
	placementTarget := CheckTarget{Kind: CheckPlacementTarget, Node: options.Node, Storage: options.Storage}
	placement := domain.Placement{Node: options.Node, Storage: options.Storage}

	run.test("task_permissions", nodeTarget, func() (string, error) {
		return gateway.TaskAccess(ctx, options.Node)
	})
	inventoryAccess := run.test("inventory_permissions", inventoryTarget, func() (string, error) {
		return gateway.InventoryAccess(ctx)
	})
	nodePresent := run.test("node_membership", nodeTarget, func() (string, error) {
		nodes, err := gateway.VisibleNodes(ctx)
		if err != nil {
			return "", err
		}
		for _, node := range nodes {
			if node == options.Node {
				return "configured node is present in the visible node list", nil
			}
		}
		return "", NewCheckFailure("configured node not found in visible node list; verify node name or permissions", false, ErrNotFound)
	})
	if nodePresent {
		run.test("node_status", nodeTarget, func() (string, error) {
			return gateway.NodeStatus(ctx, options.Node)
		})
		run.test("task_api", nodeTarget, func() (string, error) {
			return gateway.ActiveTasks(ctx, options.Node)
		})
	} else {
		run.skip("node_status", nodeTarget, "configured node was not verified in the visible node list")
		run.skip("task_api", nodeTarget, "configured node was not verified in the visible node list")
	}
	run.test("reserved_owner", CheckTarget{Kind: CheckVMTarget, VMID: options.OwnerVMID}, func() (string, error) {
		unused, err := gateway.OwnerUnused(ctx, options.OwnerVMID)
		if err == nil && !unused {
			err = domain.ErrOwnerInUse
		}
		return "reserved disk owner VMID is unused by VMs and containers", err
	})
	storageReadable := run.test("storage_read_permissions", storageTarget, func() (string, error) {
		return gateway.StorageReadAccess(ctx, placement, options.OwnerVMID)
	})
	run.test("storage_create_permission", storageTarget, func() (string, error) {
		return gateway.StorageAllocationAccess(ctx, placement)
	})
	run.test("storage_delete_permission", storageTarget, func() (string, error) {
		return gateway.StorageDeletionAccess(ctx, placement)
	})
	if nodePresent {
		run.test("storage_status", placementTarget, func() (string, error) {
			observation, err := gateway.InspectPool(ctx, placement, options.OwnerVMID)
			if err != nil {
				return "", err
			}
			if err := observation.Pool.CheckCapacity(domain.GiB); err != nil {
				if errors.Is(err, domain.ErrStorage) && observation.UnsupportedDetail != "" {
					err = NewCheckFailure(observation.UnsupportedDetail, false, err)
				}
				return "", err
			}
			return observation.Detail, nil
		})
	} else {
		run.skip("storage_status", placementTarget, "configured node was not verified in the visible node list")
	}
	if nodePresent && storageReadable {
		run.test("storage_content", placementTarget, func() (string, error) {
			count, err := gateway.ImageCount(ctx, placement, options.OwnerVMID)
			return fmt.Sprintf("reserved-owner image content is readable (%d entries)", count), err
		})
	} else {
		run.skip("storage_content", placementTarget, "configured node and storage visibility permissions were not both verified")
	}

	var entries []CheckVM
	vmListOK := false
	if nodePresent {
		vmListOK = run.test("vm_list", nodeTarget, func() (string, error) {
			var err error
			entries, err = gateway.ListVMs(ctx, options.Node)
			return fmt.Sprintf("read %d visible VM entries on the configured node", len(entries)), err
		})
	} else {
		run.skip("vm_list", nodeTarget, "configured node was not verified in the visible node list")
	}
	listedVMs := make(map[int]bool, len(entries))
	for _, entry := range entries {
		listedVMs[entry.VMID] = true
	}
	var machines []domain.VirtualMachine
	inventoryOK := false
	if inventoryAccess && vmListOK {
		inventoryOK = run.test("vm_inventory", nodeTarget, func() (string, error) {
			var err error
			machines, err = gateway.VMIdentities(ctx, options.Node, entries)
			return fmt.Sprintf("read %d visible non-template VMs with valid UUIDs", len(machines)), err
		})
	} else {
		run.skip("vm_inventory", nodeTarget, "inventory access and the visible VM list were not both verified")
	}
	for _, vmid := range options.VMIDs {
		target := CheckTarget{Kind: CheckVMTarget, VMID: vmid}
		listed := false
		if vmListOK {
			listed = run.test("vm_membership", target, func() (string, error) {
				if !listedVMs[vmid] {
					return "", NewCheckFailure("VMID not found in visible VM list for the configured node; verify VMID/node or permissions", false, ErrNotFound)
				}
				return "explicitly selected VM is present in the visible VM list on the configured node", nil
			})
		} else {
			run.skip("vm_membership", target, "visible VM list on the configured node was not verified")
		}
		readable := run.test("vm_read_permission", target, func() (string, error) {
			return gateway.VMReadAccess(ctx, vmid)
		})
		writable := run.test("vm_disk_permission", target, func() (string, error) {
			return gateway.VMDiskAccess(ctx, vmid)
		})
		var vm domain.VirtualMachine
		identityOK := false
		if readable && listed {
			identityOK = run.test("vm_identity", target, func() (string, error) {
				var err error
				vm, err = gateway.VMIdentity(ctx, options.Node, vmid)
				return "explicit VM is readable and has UUID " + vm.ID.String(), err
			})
		} else {
			run.skip("vm_identity", target, "VM read permission and membership on the configured node were not both verified")
		}
		if identityOK && inventoryOK {
			run.test("vm_uuid_unique", target, func() (string, error) {
				match, err := domain.MatchNode(options.Node, vm.ID, machines)
				if err == nil && match.VMID != vmid {
					err = domain.ErrInvalidInventory
				}
				return "UUID identifies this VM uniquely among the visible VM inventory", err
			})
		} else {
			run.skip("vm_uuid_unique", target, "explicit VM identity and visible inventory were not both verified")
		}
		if identityOK && writable {
			run.test("vm_attach_readiness", target, func() (string, error) {
				return gateway.AttachmentReadiness(ctx, vm)
			})
		} else {
			run.skip("vm_attach_readiness", target, "VM identity and disk permission were not both verified")
		}
	}
	run.results = append(run.results,
		CheckResult{Name: "inventory_scope", Target: inventoryTarget, Status: CheckWarn, Detail: "an access-filtered inventory cannot prove no hidden VMs, templates, containers or more restrictive child permissions exist; compare with an administrator's inventory"},
		CheckResult{Name: "readonly_scope", Target: CheckTarget{Kind: CheckScopeTarget, Node: options.Node}, Status: CheckWarn, Detail: "read-only checks do not test disk creation/deletion, hotplug, guest mounts or data writes; compare VM UUIDs with Kubernetes node identities and run a separate PVC/Pod smoke test"},
	)
	return run.results, nil
}

type checkRun struct {
	ctx     context.Context
	results CheckReport
	stopped bool
}

func (r *checkRun) test(name string, target CheckTarget, operation func() (string, error)) bool {
	if r.stopped {
		r.skip(name, target, "not run after authentication, connection, service failure or cancellation")
		return false
	}
	var detail string
	err := r.ctx.Err()
	if err == nil {
		detail, err = operation()
	}
	// Cancellation wins over an ordinary resource failure observed concurrently.
	if contextErr := r.ctx.Err(); contextErr != nil {
		err = contextErr
	}
	status := CheckPass
	if err != nil {
		status = CheckFail
		detail = CheckFailureDetail(err)
		if detail == "" {
			detail = "check failed with an unexpected response"
		}
		r.stopped = checkTerminal(err)
	}
	r.results = append(r.results, CheckResult{Name: name, Target: target, Status: status, Detail: detail})
	return err == nil
}

func (r *checkRun) skip(name string, target CheckTarget, detail string) {
	r.results = append(r.results, CheckResult{Name: name, Target: target, Status: CheckSkip, Detail: detail})
}

func checkTerminal(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var failure *CheckFailure
	return errors.As(err, &failure) && failure.Terminal()
}
