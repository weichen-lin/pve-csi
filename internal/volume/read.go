package volume

import (
	"context"
	"errors"

	"pve-csi.local/csi-plugin/internal/domain"
)

type Reader struct {
	storage ReadGateway
	node    string
	owner   int
}

func NewReader(storage ReadGateway, node string, owner int) (*Reader, error) {
	if storage == nil {
		return nil, errors.New("volume reads require a storage gateway")
	}
	if err := domain.ValidateOwnerScope(node, owner); err != nil {
		return nil, err
	}
	return &Reader{storage: storage, node: node, owner: owner}, nil
}

func (r *Reader) GetVolume(ctx context.Context, id domain.Identity) (domain.Volume, error) {
	if err := ctx.Err(); err != nil {
		return domain.Volume{}, err
	}
	if err := id.Validate(); err != nil {
		return domain.Volume{}, err
	}
	if id.Placement().Node != r.node || id.OwnerVMID() != r.owner {
		return domain.Volume{}, ErrNotFound
	}
	pool, err := r.storage.InspectPool(ctx, id.Placement())
	if err != nil {
		return domain.Volume{}, err
	}
	if err := pool.CheckReadable(); err != nil {
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

// ReadGateway deliberately exposes no allocation or deletion operation.
type ReadGateway interface {
	InspectPool(context.Context, domain.Placement) (PoolState, error)
	LookupVolume(context.Context, domain.Identity) (domain.Allocation, bool, error)
}
