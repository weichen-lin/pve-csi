package volume

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"pve-csi.local/csi-plugin/internal/domain"
)

// Service owns one in-flight guard per controller instance.
type Service struct {
	storage     StorageGateway
	attachments AttachmentGateway
	scope       domain.Scope
	mu          sync.Mutex
	vmMu        sync.Mutex
	inFlight    map[string]struct{}
}

func NewService(storage StorageGateway, scope domain.Scope, attachments AttachmentGateway) (*Service, error) {
	if storage == nil || attachments == nil {
		return nil, errors.New("volume service requires storage and attachment gateways")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return &Service{storage: storage, attachments: attachments, scope: scope, inFlight: make(map[string]struct{})}, nil
}

func (s *Service) Scope() domain.Scope { return s.scope }

func (s *Service) EnsureVolume(ctx context.Context, req domain.Request) (domain.Volume, error) {
	if err := req.Validate(); err != nil {
		return domain.Volume{}, AtStep("validate_request", "volume", err)
	}
	if !s.scope.Contains(req.Placement(), req.OwnerVMID) {
		return domain.Volume{}, AtStep("validate_request_scope", "volume", ErrOutsideScope)
	}
	if err := ctx.Err(); err != nil {
		return domain.Volume{}, err
	}
	if !s.acquire(req.Handle()) {
		return domain.Volume{}, ErrBusy
	}
	defer s.release(req.Handle())
	target := req.Node + "/" + req.Storage
	pool, err := s.storage.InspectPool(ctx, req.Placement(), req.OwnerVMID)
	if err != nil {
		return domain.Volume{}, err
	}
	if err := pool.CheckReady(); err != nil {
		return domain.Volume{}, AtStep("check_storage_status", target, err)
	}
	if err := s.storage.CheckIdle(ctx, req.Placement(), req.OwnerVMID); err != nil {
		return domain.Volume{}, err
	}
	if err := s.CheckReservedOwner(ctx); err != nil {
		return domain.Volume{}, err
	}
	if existing, found, err := s.observe(ctx, req); err != nil {
		return domain.Volume{}, AtStep("lookup_volume", target, err)
	} else if found {
		return existing, nil
	}
	// This observation is a preflight, not a capacity reservation.
	if err := s.storage.CheckAllocationPermission(ctx, req.Placement()); err != nil {
		return domain.Volume{}, AtStep("check_allocation_permission", target, err)
	}
	if err := pool.CheckCapacity(req.CapacityBytes); err != nil {
		return domain.Volume{}, AtStep("check_capacity", target, err)
	}
	reference, postErr := s.storage.Allocate(ctx, req)
	unexpectedID := postErr == nil && reference != req.AllocationReference()
	if unexpectedID {
		postErr = errors.New("storage allocation returned an unexpected volume ID")
	}
	// Read back after one write attempt; never retry or delete an uncertain result.
	actual, found, lookupErr := s.observe(ctx, req)
	if lookupErr == nil && found && !unexpectedID {
		return actual, nil
	}
	if lookupErr == nil && !found {
		// Explicit rejection proves failure; a lost response does not.
		if rejectedWrite(postErr) {
			return domain.Volume{}, AtStep("allocate_volume", target, postErr)
		}
		lookupErr = errors.New("expected volume is not visible in storage")
	}
	return domain.Volume{}, AtStep("confirm_volume", target, fmt.Errorf("%w: %w", ErrUnconfirmed, errors.Join(postErr, lookupErr)))
}

func (s *Service) acquire(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.inFlight[id]; busy {
		return false
	}
	s.inFlight[id] = struct{}{}
	return true
}

func (s *Service) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, id)
}

// CheckReservedOwner rechecks ownership on every operation, including after startup.
func (s *Service) CheckReservedOwner(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := strconv.Itoa(s.scope.OwnerVMID)
	unused, err := s.storage.OwnerUnused(ctx, s.scope.OwnerVMID)
	if err != nil {
		return AtStep("check_reserved_owner", target, err)
	}
	if !unused {
		return AtStep("check_reserved_owner", target, domain.ErrOwnerInUse)
	}
	return nil
}

func (s *Service) observe(ctx context.Context, req domain.Request) (domain.Volume, bool, error) {
	id, err := domain.ParseHandle(req.Handle())
	if err != nil {
		return domain.Volume{}, false, err
	}
	allocation, found, err := s.storage.LookupVolume(ctx, id)
	if err != nil || !found {
		return domain.Volume{}, false, err
	}
	volume, err := domain.Confirm(req, allocation)
	return volume, err == nil, err
}

func (s *Service) GetVolume(ctx context.Context, id domain.Identity) (domain.Volume, error) {
	if err := ctx.Err(); err != nil {
		return domain.Volume{}, err
	}
	if err := id.Validate(); err != nil {
		return domain.Volume{}, err
	}
	if !s.scope.Contains(id.Placement(), id.OwnerVMID()) {
		return domain.Volume{}, ErrNotFound
	}
	target := id.Placement().Node + "/" + id.Placement().Storage
	pool, err := s.storage.InspectPool(ctx, id.Placement(), id.OwnerVMID())
	if err != nil {
		return domain.Volume{}, err
	}
	if err := pool.CheckReady(); err != nil {
		return domain.Volume{}, AtStep("check_storage_status", target, err)
	}
	observed, found, err := s.storage.LookupVolume(ctx, id)
	if err != nil {
		return domain.Volume{}, AtStep("lookup_volume", target, err)
	}
	if !found {
		// Permission filtering can hide a volume without proving deletion.
		return domain.Volume{}, AtStep("lookup_volume", target, ErrNotFound)
	}
	result, err := domain.Restore(id, observed)
	return result, AtStep("lookup_volume", target, err)
}
