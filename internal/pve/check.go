package pve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

// CheckGateway maps read-only PVE observations to deployment checks.
type CheckGateway struct {
	client      *Client
	storage     *StorageGateway
	attachments *AttachmentGateway
}

var _ volume.CheckGateway = (*CheckGateway)(nil)

func NewCheckGateway(client *Client) (*CheckGateway, error) {
	if client == nil || client.httpClient == nil {
		return nil, errors.New("check gateway requires an initialized PVE client")
	}
	storage, err := NewStorageGateway(client)
	if err != nil {
		return nil, err
	}
	attachments, err := NewAttachmentGateway(client)
	if err != nil {
		return nil, err
	}
	return &CheckGateway{client: client, storage: storage, attachments: attachments}, nil
}

func (g *CheckGateway) TaskAccess(ctx context.Context, node string) (string, error) {
	return "Sys.Audit is effective for the token", normalizeCheckError(g.client.checkTaskAccess(ctx, node))
}

func (g *CheckGateway) InventoryAccess(ctx context.Context) (string, error) {
	return "VM.Audit is effective with propagation", normalizeCheckError(g.attachments.checkInventoryAccess(ctx))
}

func (g *CheckGateway) VisibleNodes(ctx context.Context) ([]string, error) {
	var entries []struct {
		Node string `json:"node"`
	}
	if err := g.client.getData(ctx, "/api2/json/nodes", &entries); err != nil {
		return nil, normalizeCheckError(err)
	}
	seen := make(map[string]bool, len(entries))
	nodes := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !domain.ValidNodeName(entry.Node) || seen[entry.Node] {
			return nil, normalizeCheckError(errors.New("invalid node list"))
		}
		seen[entry.Node] = true
		nodes = append(nodes, entry.Node)
	}
	return nodes, nil
}

func (g *CheckGateway) NodeStatus(ctx context.Context, node string) (string, error) {
	var state struct {
		Uptime *int64 `json:"uptime"`
	}
	err := g.client.getData(ctx, "/api2/json/nodes/"+url.PathEscape(node)+"/status", &state)
	if err == nil && (state.Uptime == nil || *state.Uptime < 0) {
		err = errors.New("invalid node status")
	}
	return "configured PVE node exists and its status is readable", normalizeCheckError(err)
}

func (g *CheckGateway) ActiveTasks(ctx context.Context, node string) (string, error) {
	_, err := g.client.activeTasks(ctx, node, url.Values{"source": {"active"}, "limit": {"100"}})
	return "active task API is readable with Sys.Audit", normalizeCheckError(err)
}

func (g *CheckGateway) OwnerUnused(ctx context.Context, owner int) (bool, error) {
	unused, err := g.client.OwnerUnused(ctx, owner)
	return unused, normalizeCheckError(err)
}

func (g *CheckGateway) StorageReadAccess(ctx context.Context, placement domain.Placement, owner int) (string, error) {
	err := g.storage.checkReadAccess(ctx, placement, owner)
	return "required effective storage-read permissions are present; storage content is checked separately", normalizeCheckError(err)
}

func (g *CheckGateway) StorageAllocationAccess(ctx context.Context, placement domain.Placement) (string, error) {
	err := g.storage.CheckAllocationPermission(ctx, placement)
	return "Datastore.AllocateSpace is effective; no disk was created", normalizeCheckError(err)
}

func (g *CheckGateway) StorageDeletionAccess(ctx context.Context, placement domain.Placement) (string, error) {
	err := g.storage.CheckDeletionPermission(ctx, placement)
	return "Datastore.Allocate is effective; no disk was deleted", normalizeCheckError(err)
}

func (g *CheckGateway) InspectPool(ctx context.Context, placement domain.Placement, owner int) (volume.CheckPoolObservation, error) {
	pool, err := g.storage.InspectPool(ctx, placement, owner)
	if err != nil {
		return volume.CheckPoolObservation{}, normalizeCheckError(err)
	}
	return volume.CheckPoolObservation{
		Pool:              pool,
		Detail:            fmt.Sprintf("enabled, active LVM-thin storage supports images; %d bytes available", pool.AvailableBytes),
		UnsupportedDetail: "storage must use LVM-thin and support images",
	}, nil
}

func (g *CheckGateway) ImageCount(ctx context.Context, placement domain.Placement, owner int) (int, error) {
	entries, err := g.client.ListImages(ctx, placement.Node, placement.Storage, owner)
	return len(entries), normalizeCheckError(err)
}

func (g *CheckGateway) ListVMs(ctx context.Context, node string) ([]volume.CheckVM, error) {
	entries, err := g.client.listGuests(ctx, node, "qemu")
	if err != nil {
		return nil, normalizeCheckError(err)
	}
	vms := make([]volume.CheckVM, 0, len(entries))
	for _, entry := range entries {
		vms = append(vms, volume.CheckVM{VMID: entry.VMID, Template: entry.Template == 1})
	}
	return vms, nil
}

// VMIdentities reuses the supplied inventory and rechecks access around config reads.
// Revoked grants must not turn filtered results into successful observations.
func (g *CheckGateway) VMIdentities(ctx context.Context, node string, entries []volume.CheckVM) ([]domain.VirtualMachine, error) {
	if err := g.attachments.checkInventoryAccess(ctx); err != nil {
		return nil, normalizeCheckError(err)
	}
	machines := make([]domain.VirtualMachine, 0, len(entries))
	for _, entry := range entries {
		if entry.Template {
			continue
		}
		if err := g.client.checkVMReadAccess(ctx, entry.VMID); err != nil {
			return nil, normalizeCheckError(err)
		}
		vm, err := g.client.vmIdentity(ctx, node, entry.VMID)
		if errors.Is(err, errMissingVMUUID) {
			continue
		}
		if err != nil {
			return nil, normalizeCheckError(err)
		}
		machines = append(machines, vm)
	}
	if err := g.attachments.checkInventoryAccess(ctx); err != nil {
		return nil, normalizeCheckError(err)
	}
	return machines, nil
}

func (g *CheckGateway) VMReadAccess(ctx context.Context, vmid int) (string, error) {
	return "VM.Audit is effective for this explicitly selected VM", normalizeCheckError(g.client.checkVMReadAccess(ctx, vmid))
}

func (g *CheckGateway) VMDiskAccess(ctx context.Context, vmid int) (string, error) {
	allowed, err := g.client.CanConfigureDisk(ctx, vmid)
	if err == nil && !allowed {
		err = missingPrivilege("/vms/"+strconv.Itoa(vmid), "VM.Config.Disk")
	}
	return "VM.Config.Disk is effective; VM configuration was not changed", normalizeCheckError(err)
}

func (g *CheckGateway) VMIdentity(ctx context.Context, node string, vmid int) (domain.VirtualMachine, error) {
	vm, err := g.client.vmIdentity(ctx, node, vmid)
	return vm, normalizeCheckError(err)
}

func (g *CheckGateway) AttachmentReadiness(ctx context.Context, vm domain.VirtualMachine) (string, error) {
	cfg, _, err := g.attachments.prepare(ctx, vm)
	if err != nil {
		return "", normalizeCheckError(err)
	}
	slot, err := freeSCSISlot(cfg)
	if err != nil {
		return "", normalizeCheckError(err)
	}
	return "no pending changes, lock or active VM configuration task; disk hotplug and VirtIO SCSI enabled; free slot " + slot, nil
}

func normalizeCheckError(err error) error {
	if err == nil {
		return nil
	}
	detail := volume.CheckFailureDetail(err)
	if detail == "" {
		detail = backendCheckHint(err)
	}
	return volume.NewCheckFailure(detail, checkFatal(err), err)
}

func checkFatal(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var transport *url.Error
	var network net.Error
	if errors.As(err, &transport) || errors.As(err, &network) {
		return true
	}
	var response *HTTPError
	return errors.As(err, &response) && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500)
}

// Keep backend response text out of diagnostic hints.
func backendCheckHint(err error) string {
	if errors.Is(err, errMissingVMUUID) {
		return "selected Kubernetes VM has no SMBIOS UUID"
	}
	var response *HTTPError
	if errors.As(err, &response) && response.StatusCode >= 500 {
		return fmt.Sprintf("PVE returned HTTP %d while reading the resource; check resource configuration and PVE service status", response.StatusCode)
	}
	var classified *volume.BackendError
	if errors.As(mapError(err), &classified) {
		return classified.Hint()
	}
	return "PVE check failed with an unexpected response"
}
