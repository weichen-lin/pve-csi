package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"pve-csi.local/csi-plugin/internal/volume/application"
	"pve-csi.local/csi-plugin/internal/volume/domain"
)

type StorageGateway struct{ client *pveapi.Client }

var _ application.StorageGateway = (*StorageGateway)(nil)
var _ application.ReadGateway = (*StorageGateway)(nil)

func NewStorageGateway(client *pveapi.Client) (*StorageGateway, error) {
	if client == nil {
		return nil, errors.New("PVE storage gateway requires a client")
	}
	return &StorageGateway{client: client}, nil
}

func (g *StorageGateway) InspectPool(ctx context.Context, placement domain.Placement) (application.PoolState, error) {
	storage, err := g.client.Storage(ctx, placement.Node, placement.Storage)
	if err != nil {
		return application.PoolState{}, mapError(err)
	}
	if storage.Type != "lvmthin" {
		return application.PoolState{}, fmt.Errorf("%w: only lvmthin is supported", application.ErrStorage)
	}
	for _, content := range strings.Split(storage.Content, ",") {
		if strings.TrimSpace(content) == "images" {
			return application.PoolState{AvailableBytes: storage.Avail}, nil
		}
	}
	return application.PoolState{}, fmt.Errorf("%w: images content is required", application.ErrStorage)
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
	var matched *pveapi.ContentEntry
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

func (g *StorageGateway) Allocate(ctx context.Context, req domain.Request) (application.AllocationReceipt, error) {
	reference, err := g.client.AllocateImage(ctx, req.Node, req.Storage, req.Filename(), req.OwnerVMID, req.CapacityBytes)
	return application.AllocationReceipt{Reference: reference}, mapError(err)
}
