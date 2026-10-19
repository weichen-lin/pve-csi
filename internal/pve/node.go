package pve

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

type NodeInventory struct{ client *Client }

var _ volume.NodeInventory = (*NodeInventory)(nil)

func NewNodeInventory(client *Client) (*NodeInventory, error) {
	if client == nil {
		return nil, errors.New("PVE node inventory requires a client")
	}
	return &NodeInventory{client: client}, nil
}

func (g *NodeInventory) checkInventoryAccess(ctx context.Context) error {
	privileges, err := g.client.permissions(ctx, "/vms")
	if err != nil {
		return volume.AtStep("check_vm_permissions", "/vms", mapError(err))
	}
	if propagate := privileges["VM.Audit"]; propagate == nil || *propagate != 1 {
		return volume.AtStep("check_vm_permissions", "/vms", missingPrivilege("/vms", "VM.Audit with propagation"))
	}
	return nil
}

func (g *NodeInventory) VirtualMachines(ctx context.Context, node string) ([]domain.VirtualMachine, error) {
	if !domain.ValidNodeName(node) {
		return nil, domain.ErrInvalidInventory
	}
	if err := g.checkInventoryAccess(ctx); err != nil {
		return nil, err
	}
	entries, err := g.client.listGuests(ctx, node, "qemu")
	if err != nil {
		return nil, volume.AtStep("list_vms", node, mapError(err))
	}
	machines := make([]domain.VirtualMachine, 0, len(entries))
	for _, entry := range entries {
		if entry.Template == 1 {
			continue
		}
		if err := g.client.checkVMReadAccess(ctx, entry.VMID); err != nil {
			return nil, err
		}
		vm, err := g.client.vmIdentity(ctx, node, entry.VMID)
		if errors.Is(err, errMissingVMUUID) {
			continue
		}
		if err != nil {
			return nil, volume.AtStep("read_vm_identity", node+"/"+strconv.Itoa(entry.VMID), mapError(err))
		}
		machines = append(machines, vm)
	}
	// A revoked grant must not turn a filtered list into a not-found result.
	if err := g.checkInventoryAccess(ctx); err != nil {
		return nil, err
	}
	return machines, nil
}

type vmEntry struct {
	VMID     int `json:"vmid"`
	Template int `json:"template"`
}

func (c *Client) listGuests(ctx context.Context, node, kind string) ([]vmEntry, error) {
	if !domain.ValidNodeName(node) || (kind != "qemu" && kind != "lxc") {
		return nil, domain.ErrInvalidInventory
	}
	var entries []vmEntry
	if err := c.getData(ctx, "/api2/json/nodes/"+url.PathEscape(node)+"/"+kind, &entries); err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(entries))
	for _, entry := range entries {
		if !domain.ValidVMID(entry.VMID) || seen[entry.VMID] || (entry.Template != 0 && entry.Template != 1) {
			return nil, domain.ErrInvalidInventory
		}
		seen[entry.VMID] = true
	}
	return entries, nil
}

func (c *Client) checkVMReadAccess(ctx context.Context, vmid int) error {
	path := "/vms/" + strconv.Itoa(vmid)
	privileges, err := c.permissions(ctx, path)
	if err != nil {
		return volume.AtStep("check_vm_permissions", path, mapError(err))
	}
	if _, allowed := privileges["VM.Audit"]; !allowed {
		return volume.AtStep("check_vm_permissions", path, missingPrivilege(path, "VM.Audit"))
	}
	return nil
}

func (c *Client) vmIdentity(ctx context.Context, node string, vmid int) (domain.VirtualMachine, error) {
	var config vmConfig
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config?current=1", url.PathEscape(node), vmid)
	if err := c.getData(ctx, path, &config); err != nil {
		return domain.VirtualMachine{}, err
	}
	if config.Template != 0 {
		return domain.VirtualMachine{}, domain.ErrInvalidInventory
	}
	id, err := config.nodeID()
	if err != nil {
		return domain.VirtualMachine{}, err
	}
	return domain.VirtualMachine{Node: node, VMID: vmid, ID: id}, nil
}
