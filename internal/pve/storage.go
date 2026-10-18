package pve

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

type StorageGateway struct{ client *Client }

var _ volume.StorageGateway = (*StorageGateway)(nil)
var _ volume.ReadGateway = (*StorageGateway)(nil)

func NewStorageGateway(client *Client) (*StorageGateway, error) {
	if client == nil {
		return nil, errors.New("PVE storage gateway requires a client")
	}
	return &StorageGateway{client: client}, nil
}

func (g *StorageGateway) InspectPool(ctx context.Context, placement domain.Placement) (volume.PoolState, error) {
	permissions, err := g.client.StoragePermissions(ctx, placement.Storage)
	if err != nil {
		return volume.PoolState{}, mapError(err)
	}
	storage, err := g.client.Storage(ctx, placement.Node, placement.Storage)
	if err != nil {
		return volume.PoolState{}, mapError(err)
	}
	supportsImages := false
	for _, content := range strings.Split(storage.Content, ",") {
		if strings.TrimSpace(content) == "images" {
			supportsImages = true
			break
		}
	}
	return volume.PoolState{
		Pool: domain.Pool{
			Enabled: storage.Enabled, Active: storage.Active,
			SupportsVolumes: storage.Type == "lvmthin" && supportsImages,
			AvailableBytes:  storage.Avail,
		},
		// PVE status/content GET accepts either privilege; allocation requires
		// AllocateSpace. Keep that backend-specific rule out of volume.
		Access: volume.StorageAccess{
			CanInspect:  permissions.Audit || permissions.AllocateSpace,
			CanAllocate: permissions.AllocateSpace,
		},
	}, nil
}

func (g *StorageGateway) Lookup(ctx context.Context, req domain.Request) (domain.Allocation, bool, error) {
	return g.lookup(ctx, req.Placement(), req.OwnerVMID, req.AllocationReference())
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

func (g *StorageGateway) Allocate(ctx context.Context, req domain.Request) (volume.AllocationReceipt, error) {
	reference, err := g.client.AllocateImage(ctx, req.Node, req.Storage, req.Filename(), req.OwnerVMID, req.CapacityBytes)
	return volume.AllocationReceipt{Reference: reference}, mapError(err)
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	kind := volume.FailureInternal
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			kind = volume.FailureUnauthenticated
		case http.StatusForbidden:
			kind = volume.FailurePermissionDenied
		case http.StatusNotFound:
			kind = volume.FailureNotFound
		default:
			if httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500 {
				kind = volume.FailureUnavailable
			}
		}
	}
	return volume.NewBackendError(kind, err)
}
