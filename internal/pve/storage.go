package pve

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

type StorageGateway struct{ client *Client }

var _ volume.StorageGateway = (*StorageGateway)(nil)

func NewStorageGateway(client *Client) (*StorageGateway, error) {
	if client == nil {
		return nil, errors.New("PVE storage gateway requires a client")
	}
	return &StorageGateway{client: client}, nil
}

func (g *StorageGateway) InspectPool(ctx context.Context, placement domain.Placement, owner int) (domain.Pool, error) {
	if err := g.checkReadAccess(ctx, placement, owner); err != nil {
		return domain.Pool{}, err
	}
	storage, err := g.client.Storage(ctx, placement.Node, placement.Storage)
	if err != nil {
		return domain.Pool{}, volume.AtStep("check_storage_status", placement.Node+"/"+placement.Storage, mapError(err))
	}
	supportsImages := false
	for _, content := range strings.Split(storage.Content, ",") {
		if strings.TrimSpace(content) == "images" {
			supportsImages = true
			break
		}
	}
	return domain.Pool{
		Enabled: storage.Enabled, Active: storage.Active,
		SupportsVolumes: storage.Type == "lvmthin" && supportsImages,
		AvailableBytes:  storage.Avail,
	}, nil
}

func (g *StorageGateway) CheckAllocationPermission(ctx context.Context, placement domain.Placement) error {
	permissions, err := g.client.StoragePermissions(ctx, placement.Storage)
	if err != nil {
		return mapError(err)
	}
	if !permissions.AllocateSpace {
		return missingPrivilege("/storage/"+placement.Storage, "Datastore.AllocateSpace")
	}
	return nil
}

func (g *StorageGateway) checkReadAccess(ctx context.Context, placement domain.Placement, owner int) error {
	path := "/storage/" + placement.Storage
	permissions, err := g.client.StoragePermissions(ctx, placement.Storage)
	if err != nil {
		return volume.AtStep("check_storage_permissions", path, mapError(err))
	}
	// Content/status endpoints require Audit or AllocateSpace. Individual disk
	// visibility additionally requires Allocate OR (Audit AND VM.Config.Disk).
	if !permissions.Audit && !permissions.AllocateSpace || !permissions.Allocate && !permissions.Audit {
		return volume.AtStep("check_storage_permissions", path, missingPrivilege(path, "Datastore.Audit"))
	}
	if !permissions.Allocate {
		path = "/vms/" + strconv.Itoa(owner)
		allowed, err := g.client.CanConfigureDisk(ctx, owner)
		if err != nil {
			return volume.AtStep("check_owner_permissions", path, mapError(err))
		}
		if !allowed {
			return volume.AtStep("check_owner_permissions", path, missingPrivilege(path, "VM.Config.Disk"))
		}
	}
	return nil
}

func (g *StorageGateway) LookupVolume(ctx context.Context, id domain.Identity) (domain.Allocation, bool, error) {
	if err := id.Validate(); err != nil {
		return domain.Allocation{}, false, err
	}
	return g.lookup(ctx, id.Placement(), id.OwnerVMID(), id.Reference())
}

func (g *StorageGateway) lookup(ctx context.Context, placement domain.Placement, owner int, reference string) (domain.Allocation, bool, error) {
	entries, err := g.client.ListImages(ctx, placement.Node, placement.Storage, owner)
	if err != nil {
		return domain.Allocation{}, false, mapError(err)
	}
	var matched *ContentEntry
	for i := range entries {
		entry := &entries[i]
		if strings.TrimSpace(entry.VolumeID) == "" {
			return domain.Allocation{}, false, errors.New("PVE storage content contains an entry without a volume ID")
		}
		if entry.VolumeID != reference {
			continue
		}
		if matched != nil {
			return domain.Allocation{}, false, errors.New("PVE storage content contains duplicate volume IDs")
		}
		matched = entry
	}
	if matched == nil {
		// A permission change can turn an existing disk into an empty list.
		// Recheck before treating that observation as absence.
		if err := g.checkReadAccess(ctx, placement, owner); err != nil {
			return domain.Allocation{}, false, err
		}
		return domain.Allocation{}, false, nil
	}
	if matched.Format != "raw" || matched.Content != "images" {
		return domain.Allocation{}, false, domain.ErrConflict
	}
	return domain.Allocation{
		Reference: matched.VolumeID, Placement: placement,
		OwnerVMID: matched.VMID, CapacityBytes: matched.Size,
	}, true, nil
}

func (g *StorageGateway) Allocate(ctx context.Context, req domain.Request) (string, error) {
	reference, err := g.client.AllocateImage(ctx, req.Node, req.Storage, req.Filename(), req.OwnerVMID, req.CapacityBytes)
	return reference, mapError(err)
}

func (g *StorageGateway) CheckIdle(ctx context.Context, placement domain.Placement, owner int) error {
	target := strconv.Itoa(owner) + "@" + placement.Storage
	active, err := g.client.HasActiveDeletion(ctx, placement.Node, target)
	if err != nil {
		return volume.AtStep("check_pending_deletion", target, mapError(err))
	}
	if active {
		return volume.AtStep("check_pending_deletion", target, volume.ErrBusy)
	}
	return nil
}

func (g *StorageGateway) CheckDeletionPermission(ctx context.Context, placement domain.Placement) error {
	path := "/storage/" + placement.Storage
	permissions, err := g.client.StoragePermissions(ctx, placement.Storage)
	if err != nil {
		return mapError(err)
	}
	if !permissions.Allocate {
		return missingPrivilege(path, "Datastore.Allocate")
	}
	return nil
}

func (g *StorageGateway) OwnerUnused(ctx context.Context, owner int) (bool, error) {
	unused, err := g.client.OwnerUnused(ctx, owner)
	return unused, mapError(err)
}

func (g *StorageGateway) Delete(ctx context.Context, id domain.Identity) error {
	if err := id.Validate(); err != nil {
		return err
	}
	placement := id.Placement()
	upid, err := g.client.DeleteImage(ctx, placement.Node, placement.Storage, id.Reference())
	if err != nil {
		return volume.AtStep("delete_volume", id.Reference(), mapError(err))
	}
	// A caller without a deadline must not keep a local operation locked forever.
	taskCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	taskID := strconv.Itoa(id.OwnerVMID()) + "@" + placement.Storage
	for {
		task, err := g.client.DeletionStatus(taskCtx, placement.Node, upid, taskID)
		if err != nil {
			return volume.AtStep("wait_deletion_task", id.Reference(), errors.Join(volume.ErrDeleteUnconfirmed, mapError(err)))
		}
		if task.Status == "stopped" {
			if task.ExitStatus != "OK" {
				return volume.AtStep("wait_deletion_task", id.Reference(), volume.ErrDeleteFailed)
			}
			return nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-taskCtx.Done():
			timer.Stop()
			return volume.AtStep("wait_deletion_task", id.Reference(), errors.Join(volume.ErrDeleteUnconfirmed, taskCtx.Err()))
		case <-timer.C:
		}
	}
}
