package volume

import (
	"context"
	"errors"
	"testing"

	"pve-csi.local/csi-plugin/internal/domain"
)

type storageStub struct {
	pool                                                     domain.Pool
	allocation                                               *domain.Allocation
	allocationPermission                                     error
	ownerUnused                                              bool
	ownerErr                                                 error
	allocateErr                                              error
	deleteErr                                                error
	allocationChecks, allocations, deletionChecks, deletions int
}

func (s *storageStub) InspectPool(context.Context, domain.Placement, int) (domain.Pool, error) {
	return s.pool, nil
}
func (s *storageStub) LookupVolume(context.Context, domain.Identity) (domain.Allocation, bool, error) {
	if s.allocation == nil {
		return domain.Allocation{}, false, nil
	}
	return *s.allocation, true, nil
}
func (s *storageStub) CheckIdle(context.Context, domain.Placement, int) error { return nil }
func (s *storageStub) CheckAllocationPermission(context.Context, domain.Placement) error {
	s.allocationChecks++
	return s.allocationPermission
}
func (s *storageStub) Allocate(_ context.Context, req domain.Request) (string, error) {
	s.allocations++
	if s.allocateErr != nil {
		return "", s.allocateErr
	}
	s.allocation = &domain.Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes}
	return s.allocation.Reference, nil
}
func (s *storageStub) CheckDeletionPermission(context.Context, domain.Placement) error {
	s.deletionChecks++
	return nil
}
func (s *storageStub) OwnerUnused(context.Context, int) (bool, error) {
	return s.ownerUnused, s.ownerErr
}
func (s *storageStub) Delete(context.Context, domain.Identity) error {
	s.deletions++
	s.allocation = nil
	return s.deleteErr
}

func volumeFixture(t *testing.T) (*Service, *storageStub, domain.Request, domain.Identity) {
	t.Helper()
	req, err := domain.NewRequest("test", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseHandle(req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	storage := &storageStub{pool: domain.Pool{Enabled: true, Active: true, SupportsVolumes: true, AvailableBytes: 10 * domain.GiB}, ownerUnused: true}
	service, err := NewService(storage, domain.Scope{Node: "pve", OwnerVMID: 7777}, &attachmentStub{})
	if err != nil {
		t.Fatal(err)
	}
	return service, storage, req, id
}

func TestAllocationPolicy(t *testing.T) {
	t.Run("existing disk does not need allocation permission or spare capacity", func(t *testing.T) {
		service, storage, req, _ := volumeFixture(t)
		storage.allocation = &domain.Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes}
		storage.pool.AvailableBytes = 0
		storage.allocationPermission = NewBackendError(FailurePermissionDenied, "allocation denied", nil)
		if _, err := service.EnsureVolume(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if storage.allocationChecks != 0 || storage.allocations != 0 {
			t.Fatal("idempotent lookup attempted allocation")
		}
	})
	t.Run("new disk checks permission before writing", func(t *testing.T) {
		service, storage, req, _ := volumeFixture(t)
		denied := NewBackendError(FailurePermissionDenied, "allocation denied", nil)
		storage.allocationPermission = denied
		_, err := service.EnsureVolume(context.Background(), req)
		var step *StepError
		if !errors.Is(err, denied) || !errors.As(err, &step) || step.Step() != "check_allocation_permission" {
			t.Fatalf("unexpected error: %v", err)
		}
		if storage.allocationChecks != 1 || storage.allocations != 0 {
			t.Fatal("denied allocation reached write")
		}
	})
	t.Run("authorized allocation is confirmed", func(t *testing.T) {
		service, storage, req, _ := volumeFixture(t)
		v, err := service.EnsureVolume(context.Background(), req)
		if err != nil || v.ID() != req.Handle() || storage.allocations != 1 || storage.allocationChecks != 1 {
			t.Fatalf("allocation: %v, %v", v, err)
		}
	})
	for _, tc := range []struct {
		name      string
		kind      FailureKind
		uncertain bool
	}{
		{"unauthenticated", FailureUnauthenticated, false},
		{"denied", FailurePermissionDenied, false},
		{"missing endpoint", FailureNotFound, false},
		{"connection lost", FailureUnavailable, true},
		{"invalid response", FailureInternal, true},
	} {
		t.Run("write outcome/"+tc.name, func(t *testing.T) {
			service, storage, req, _ := volumeFixture(t)
			storage.allocateErr = NewBackendError(tc.kind, "", nil)
			_, err := service.EnsureVolume(context.Background(), req)
			if !errors.Is(err, storage.allocateErr) || errors.Is(err, ErrUnconfirmed) != tc.uncertain || storage.allocations != 1 {
				t.Fatalf("write outcome: %v", err)
			}
		})
	}
}

func TestDeletionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unused      bool
		cause, want error
	}{
		{"owner in use", false, nil, domain.ErrOwnerInUse},
		{"owner lookup failed", true, context.DeadlineExceeded, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, storage, req, id := volumeFixture(t)
			storage.allocation = &domain.Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes}
			storage.ownerUnused, storage.ownerErr = tc.unused, tc.cause
			if err := service.DeleteVolume(context.Background(), id); !errors.Is(err, tc.want) {
				t.Fatalf("delete error: %v", err)
			}
			if storage.deletions != 0 {
				t.Fatal("unsafe deletion reached backend")
			}
		})
	}
	t.Run("absent disk requires no deletion permission", func(t *testing.T) {
		service, storage, _, id := volumeFixture(t)
		storage.ownerUnused = false
		if err := service.DeleteVolume(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if storage.deletionChecks != 0 || storage.deletions != 0 {
			t.Fatal("absent disk triggered deletion")
		}
	})
	t.Run("task polling denied does not prove delete rejected", func(t *testing.T) {
		service, storage, req, id := volumeFixture(t)
		storage.allocation = &domain.Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes}
		storage.deleteErr = errors.Join(ErrDeleteUnconfirmed, NewBackendError(FailurePermissionDenied, "task status denied", nil))
		if err := service.DeleteVolume(context.Background(), id); !errors.Is(err, ErrDeleteUnconfirmed) {
			t.Fatalf("unknown deletion treated as complete: %v", err)
		}
		if storage.deletions != 1 {
			t.Fatal("expected one delete attempt")
		}
	})
}

func TestServiceRequiresAllGateways(t *testing.T) {
	scope := domain.Scope{Node: "pve", OwnerVMID: 7777}
	if _, err := NewService(nil, scope, &attachmentStub{}); err == nil {
		t.Fatal("missing storage accepted")
	}
	if _, err := NewService(&storageStub{}, scope, nil); err == nil {
		t.Fatal("missing attachment inspection accepted")
	}
}

func TestStartupOwnerCheckDoesNotAuthorizeLaterWrites(t *testing.T) {
	for _, operation := range []string{"create", "attach", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, storage, req, id := volumeFixture(t)
			uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
			a := s.attachments.(*attachmentStub)
			a.machines = []domain.VirtualMachine{{Node: "pve", VMID: 100, ID: uuid}}
			if operation != "create" {
				if _, err := s.EnsureVolume(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CheckReservedOwner(context.Background()); err != nil {
				t.Fatal(err)
			}
			storage.ownerUnused = false
			before := storage.allocations
			var err error
			if operation == "create" {
				_, err = s.EnsureVolume(context.Background(), req)
			} else if operation == "attach" {
				_, err = s.Publish(context.Background(), id, uuid.String())
			} else {
				err = s.DeleteVolume(context.Background(), id)
			}
			if !errors.Is(err, domain.ErrOwnerInUse) || storage.allocations != before || a.attaches != 0 || storage.deletions != 0 {
				t.Fatalf("reserved owner reused: %v", err)
			}
		})
	}
}
