package volume

import (
	"context"
	"errors"
	"testing"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
)

// Commit the effect independently of the response/read-back. Restarting Service
// preserves this backend, just as restarting the controller preserves PVE state.
type lostReplyStorage struct {
	*storageStub
	failReadback  bool
	pendingDelete bool
}

func (s *lostReplyStorage) Allocate(ctx context.Context, req domain.Request) (string, error) {
	_, err := s.storageStub.Allocate(ctx, req)
	if err != nil {
		return "", err
	}
	return "", context.DeadlineExceeded
}
func (s *lostReplyStorage) LookupVolume(ctx context.Context, id domain.Identity) (domain.Allocation, bool, error) {
	if s.allocations > 0 && s.failReadback {
		return domain.Allocation{}, false, context.DeadlineExceeded
	}
	return s.storageStub.LookupVolume(ctx, id)
}
func (s *lostReplyStorage) CheckIdle(context.Context, domain.Placement, int) error {
	if s.pendingDelete {
		return ErrBusy
	}
	return nil
}
func (s *lostReplyStorage) Delete(ctx context.Context, id domain.Identity) error {
	if err := s.storageStub.Delete(ctx, id); err != nil {
		return err
	}
	s.pendingDelete = true
	return errors.Join(ErrDeleteUnconfirmed, context.DeadlineExceeded)
}

func TestAllocationLostReplyAndRestartNeverDuplicateDisk(t *testing.T) {
	for _, readFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-back succeeds", true: "read-back fails"}[readFails], func(t *testing.T) {
			s, disk, req, _ := volumeFixture(t)
			backend := &lostReplyStorage{storageStub: disk, failReadback: readFails}
			s.storage = backend
			_, err := s.EnsureVolume(context.Background(), req)
			if readFails && !errors.Is(err, ErrUnconfirmed) || !readFails && err != nil {
				t.Fatal(err)
			}
			if disk.allocation == nil || disk.allocations != 1 || disk.deletions != 0 {
				t.Fatal("uncertain disk discarded")
			}
			backend.failReadback = false
			restarted, err := NewService(backend, s.scope, s.attachments)
			if err != nil {
				t.Fatal(err)
			}
			v, err := restarted.EnsureVolume(context.Background(), req)
			if err != nil || v.ID() != req.Handle() || disk.allocations != 1 || disk.deletions != 0 {
				t.Fatalf("retry mutated disk: %v", err)
			}
		})
	}
}

func TestDeletionLostReplyWaitsForWorkerAcrossRestart(t *testing.T) {
	s, disk, req, id := volumeFixture(t)
	if _, err := s.EnsureVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	backend := &lostReplyStorage{storageStub: disk}
	s.storage = backend
	if err := s.DeleteVolume(context.Background(), id); !errors.Is(err, ErrDeleteUnconfirmed) {
		t.Fatal(err)
	}
	restarted, _ := NewService(backend, s.scope, s.attachments)
	if err := restarted.DeleteVolume(context.Background(), id); !errors.Is(err, ErrBusy) {
		t.Fatalf("active delete treated as finished: %v", err)
	}
	backend.pendingDelete = false
	if err := restarted.DeleteVolume(context.Background(), id); err != nil || disk.deletions != 1 {
		t.Fatalf("delete repeated: %v", err)
	}
}

type lostReplyAttachment struct {
	*attachmentStub
	readbackErr error
	changed     bool
}

func (a *lostReplyAttachment) Attach(ctx context.Context, id domain.Identity, vm domain.VirtualMachine) error {
	if err := a.attachmentStub.Attach(ctx, id, vm); err != nil {
		return err
	}
	a.changed = true
	return errors.Join(ErrAttachmentUnconfirmed, context.DeadlineExceeded)
}
func (a *lostReplyAttachment) Detach(ctx context.Context, id domain.Identity, ref domain.DiskReference) error {
	if err := a.attachmentStub.Detach(ctx, id, ref); err != nil {
		return err
	}
	a.changed = true
	return errors.Join(ErrAttachmentUnconfirmed, context.DeadlineExceeded)
}
func (a *lostReplyAttachment) References(ctx context.Context, id domain.Identity) (domain.References, error) {
	if a.changed && a.readbackErr != nil {
		return nil, a.readbackErr
	}
	return a.attachmentStub.References(ctx, id)
}

func TestAttachmentLostReplyAndReadbackFailureAcrossRestart(t *testing.T) {
	for _, failure := range []error{nil, context.DeadlineExceeded} {
		s, _, req, id := volumeFixture(t)
		uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
		a := &lostReplyAttachment{attachmentStub: &attachmentStub{machines: []domain.VirtualMachine{{Node: "pve", VMID: 100, ID: uuid}}}, readbackErr: failure}
		s.attachments = a
		if _, err := s.EnsureVolume(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Publish(context.Background(), id, uuid.String()); !errors.Is(err, ErrAttachmentUnconfirmed) {
			t.Fatal(err)
		}
		if len(a.refs) != 1 {
			t.Fatal("test did not commit attachment")
		}
		a.readbackErr = nil
		restarted, _ := NewService(s.storage, s.scope, a)
		if _, err := restarted.Publish(context.Background(), id, uuid.String()); err != nil || a.attaches != 1 {
			t.Fatalf("duplicate attach: %v", err)
		}
		a.changed = false
		a.readbackErr = failure
		if err := restarted.Unpublish(context.Background(), id, uuid.String()); !errors.Is(err, ErrAttachmentUnconfirmed) {
			t.Fatal(err)
		}
		a.readbackErr = nil
		restarted, _ = NewService(s.storage, s.scope, a)
		if err := restarted.Unpublish(context.Background(), id, uuid.String()); err != nil || a.detaches != 1 || len(a.refs) != 0 {
			t.Fatalf("duplicate detach: %v", err)
		}
	}
}

type blockedAllocation struct {
	StorageGateway
	entered, release chan struct{}
}

func (s *blockedAllocation) Allocate(ctx context.Context, req domain.Request) (string, error) {
	close(s.entered)
	<-s.release
	return s.StorageGateway.Allocate(ctx, req)
}

func awaitOperation(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not reach barrier")
	}
}

func TestConcurrentRequestsForSameVolumeNeverAllocateTwice(t *testing.T) {
	s, storage, req, id := volumeFixture(t)
	gate := &blockedAllocation{StorageGateway: storage, entered: make(chan struct{}), release: make(chan struct{})}
	s.storage = gate
	done := make(chan error, 1)
	go func() { _, err := s.EnsureVolume(context.Background(), req); done <- err }()
	awaitOperation(t, gate.entered)
	_, createErr := s.EnsureVolume(context.Background(), req)
	deleteErr := s.DeleteVolume(context.Background(), id)
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(createErr, ErrBusy) || !errors.Is(deleteErr, ErrBusy) || storage.allocations != 1 || storage.deletions != 0 {
		t.Fatalf("concurrent mutation: %v %v", createErr, deleteErr)
	}
}

type blockedAttach struct {
	*attachmentStub
	entered, release chan struct{}
}

func (a *blockedAttach) Attach(ctx context.Context, id domain.Identity, vm domain.VirtualMachine) error {
	close(a.entered)
	<-a.release
	return a.attachmentStub.Attach(ctx, id, vm)
}

func TestConcurrentVolumesCannotCompeteForVMSlot(t *testing.T) {
	s, _, req, first := volumeFixture(t)
	uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	a := &blockedAttach{attachmentStub: &attachmentStub{machines: []domain.VirtualMachine{{Node: "pve", VMID: 100, ID: uuid}}}, entered: make(chan struct{}), release: make(chan struct{})}
	s.attachments = a
	if _, err := s.EnsureVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req, err := domain.NewRequest("second", req.Placement(), req.OwnerVMID, req.CapacityBytes, req.CapacityLimitBytes)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := domain.ParseHandle(req.Handle())
	done := make(chan error, 1)
	go func() { _, err := s.Publish(context.Background(), first, uuid.String()); done <- err }()
	awaitOperation(t, a.entered)
	_, err = s.Publish(context.Background(), second, uuid.String())
	close(a.release)
	if firstErr := <-done; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, ErrBusy) || a.attaches != 1 {
		t.Fatalf("competing VM write allowed: %v", err)
	}
}

func TestAttachmentValidatesRequestBeforeBackendReads(t *testing.T) {
	// Calling any backend method before rejecting these inputs would panic.
	s, err := NewService(struct{ StorageGateway }{}, domain.Scope{Node: "pve", OwnerVMID: 7777}, struct{ AttachmentGateway }{})
	if err != nil {
		t.Fatal(err)
	}
	req, err := domain.NewRequest("one", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHandle(req.Handle())
	if _, err := s.Publish(context.Background(), id, "not-a-uuid"); !errors.Is(err, domain.ErrInvalidNodeID) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Publish(ctx, id, "153a7c9e-588b-483f-884a-4eba01bc160a"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Unpublish(ctx, id, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
