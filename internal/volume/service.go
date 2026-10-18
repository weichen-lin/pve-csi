package volume

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"pve-csi.local/csi-plugin/internal/domain"
)

// Service owns one in-flight guard per controller instance.
type Service struct {
	storage  StorageGateway
	mu       sync.Mutex
	inFlight map[string]struct{}
}

func NewService(storage StorageGateway) (*Service, error) {
	if storage == nil {
		return nil, errors.New("provisioning requires a storage gateway")
	}
	return &Service{storage: storage, inFlight: make(map[string]struct{})}, nil
}

func (s *Service) EnsureVolume(ctx context.Context, req domain.Request) (domain.Volume, error) {
	if err := req.Validate(); err != nil {
		return domain.Volume{}, AtStep("validate_request", "volume", err)
	}
	if err := ctx.Err(); err != nil {
		return domain.Volume{}, err
	}
	s.mu.Lock()
	_, busy := s.inFlight[req.Name]
	if !busy {
		s.inFlight[req.Name] = struct{}{}
	}
	s.mu.Unlock()
	if busy {
		return domain.Volume{}, ErrBusy
	}
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, req.Name)
		s.mu.Unlock()
	}()
	target := req.Node + "/" + req.Storage
	pool, err := s.storage.InspectPool(ctx, req.Placement(), req.OwnerVMID)
	if err != nil {
		return domain.Volume{}, err
	}
	if err := pool.CheckReadable(); err != nil {
		return domain.Volume{}, AtStep("check_storage_status", target, err)
	}
	if existing, found, err := s.observe(ctx, req); err != nil {
		return domain.Volume{}, AtStep("lookup_volume", target, err)
	} else if found {
		return existing, nil
	}
	// This observation is a preflight, not a capacity reservation.
	if err := pool.CheckAllocationPermission(); err != nil {
		return domain.Volume{}, AtStep("check_allocation_permission", target, err)
	}
	if err := pool.Pool.CheckCapacity(req.CapacityBytes); err != nil {
		return domain.Volume{}, AtStep("check_capacity", target, err)
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
		// Authentication/authorization/route rejection is a definite failure,
		// unlike a lost connection or 5xx after the server accepted the write.
		var backendErr *BackendError
		if errors.As(postErr, &backendErr) && (backendErr.Kind == FailureUnauthenticated || backendErr.Kind == FailurePermissionDenied || backendErr.Kind == FailureNotFound) {
			return domain.Volume{}, AtStep("allocate_volume", target, postErr)
		}
		lookupErr = errors.New("expected volume is not visible in PVE storage content")
	}
	return domain.Volume{}, AtStep("confirm_volume", target, fmt.Errorf("%w: %w", ErrUnconfirmed, errors.Join(postErr, lookupErr)))
}

func (s *Service) observe(ctx context.Context, req domain.Request) (domain.Volume, bool, error) {
	allocation, found, err := s.storage.Lookup(ctx, req)
	if err != nil || !found {
		return domain.Volume{}, false, err
	}
	volume, err := domain.Confirm(req, allocation)
	return volume, err == nil, err
}

type AllocationReceipt struct{ Reference string }

// StorageGateway is owned by this use case. Lookup false means not observed;
// permission filtering or a remote write can still hide an existing allocation.
type StorageGateway interface {
	InspectPool(context.Context, domain.Placement, int) (PoolState, error)
	Lookup(context.Context, domain.Request) (domain.Allocation, bool, error)
	Allocate(context.Context, domain.Request) (AllocationReceipt, error)
}
