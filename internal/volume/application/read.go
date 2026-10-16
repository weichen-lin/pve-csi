package application

import (
	"context"
	"errors"

	"pve-csi.local/csi-plugin/internal/volume/domain"
)

type Reader struct {
	storage   ReadGateway
	placement domain.Placement
	owner     int
}

func NewReader(storage ReadGateway, placement domain.Placement, owner int) (*Reader, error) {
	if storage == nil {
		return nil, errors.New("volume reads require a storage gateway")
	}
	probe := domain.Request{Name: "configuration-check", Node: placement.Node, Storage: placement.Storage, OwnerVMID: owner, CapacityBytes: domain.GiB}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	if _, err := domain.ParseHandle(probe.Handle()); err != nil {
		return nil, err
	}
	return &Reader{storage: storage, placement: placement, owner: owner}, nil
}

func (r *Reader) GetVolume(ctx context.Context, id domain.Identity) (domain.Volume, error) {
	if err := ctx.Err(); err != nil {
		return domain.Volume{}, err
	}
	if err := id.Validate(); err != nil {
		return domain.Volume{}, err
	}
	if id.Placement() != r.placement || id.OwnerVMID() != r.owner {
		return domain.Volume{}, ErrNotFound
	}
	if _, err := r.storage.InspectPool(ctx, r.placement); err != nil {
		return domain.Volume{}, err
	}
	observed, found, err := r.storage.LookupVolume(ctx, id)
	if err != nil {
		return domain.Volume{}, err
	}
	if !found {
		// Permission filtering can also hide volumes. This is not evidence
		// that deletion occurred or permission to allocate another volume.
		return domain.Volume{}, ErrNotFound
	}
	return domain.Restore(id, observed)
}
