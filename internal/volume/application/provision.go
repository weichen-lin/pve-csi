package application

import (
	"context"
	"errors"
	"fmt"

	"pve-csi.local/csi-plugin/internal/volume/domain"
)

type Service struct{ storage StorageGateway }

func NewService(storage StorageGateway) (*Service, error) {
	if storage == nil {
		return nil, errors.New("provisioning requires a storage gateway")
	}
	return &Service{storage: storage}, nil
}

func (s *Service) EnsureVolume(ctx context.Context, req domain.Request) (domain.Volume, error) {
	if err := req.Validate(); err != nil {
		return domain.Volume{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Volume{}, err
	}
	pool, err := s.storage.InspectPool(ctx, req.Placement())
	if err != nil {
		return domain.Volume{}, err
	}
	if existing, found, err := s.observe(ctx, req); err != nil {
		return domain.Volume{}, err
	} else if found {
		return existing, nil
	}
	// This observation is a preflight, not a capacity reservation.
	if req.CapacityBytes > pool.AvailableBytes {
		return domain.Volume{}, ErrCapacity
	}
	receipt, postErr := s.storage.Allocate(ctx, req)
	unexpectedID := postErr == nil && receipt.Reference != req.AllocationReference()
	if unexpectedID {
		postErr = errors.New("PVE allocation returned an unexpected volume ID")
	}
	// Always read back after the one allocation attempt, using the caller's
	// context. Never retry the write, rename or delete an uncertain result here.
	actual, found, lookupErr := s.observe(ctx, req)
	if lookupErr == nil && found && !unexpectedID {
		return actual, nil
	}
	if lookupErr == nil && !found {
		lookupErr = errors.New("expected volume is not visible in PVE storage content")
	}
	return domain.Volume{}, fmt.Errorf("%w; retry with the same name, node, storage and owner: %w", ErrUnconfirmed, errors.Join(postErr, lookupErr))
}

func (s *Service) observe(ctx context.Context, req domain.Request) (domain.Volume, bool, error) {
	allocation, found, err := s.storage.Lookup(ctx, req)
	if err != nil || !found {
		return domain.Volume{}, false, err
	}
	volume, err := domain.Confirm(req, allocation)
	return volume, err == nil, err
}
