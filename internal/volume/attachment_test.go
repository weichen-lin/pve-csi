package volume

import (
	"context"
	"errors"
	"testing"

	"pve-csi.local/csi-plugin/internal/domain"
)

type attachmentStub struct {
	machines           []domain.VirtualMachine
	refs               domain.References
	readErr, writeErr  error
	attaches, detaches int
	inventoryReads     int
}

func (a *attachmentStub) VirtualMachines(context.Context, string) ([]domain.VirtualMachine, error) {
	a.inventoryReads++
	return a.machines, a.readErr
}
func (a *attachmentStub) References(context.Context, domain.Identity) (domain.References, error) {
	return a.refs, a.readErr
}
func (a *attachmentStub) Attach(_ context.Context, id domain.Identity, vm domain.VirtualMachine) error {
	a.attaches++
	if a.writeErr == nil {
		a.refs = domain.References{{Source: vm.ReferenceSource(), Slot: "scsi1", Serial: id.Serial(), State: domain.ReferenceAttached}}
	}
	return a.writeErr
}
func (a *attachmentStub) Detach(context.Context, domain.Identity, domain.DiskReference) error {
	a.detaches++
	if a.writeErr == nil {
		a.refs = nil
	}
	return a.writeErr
}

func TestVolumeLifecyclePolicy(t *testing.T) {
	ctx := context.Background()
	s, storage, req, id := volumeFixture(t)
	uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	vm := domain.VirtualMachine{Node: "pve", VMID: 100, ID: uuid}
	a := &attachmentStub{machines: []domain.VirtualMachine{vm}}
	s.attachments = a
	if _, err := s.EnsureVolume(ctx, req); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if serial, err := s.Publish(ctx, id, uuid.String()); err != nil || serial != id.Serial() {
			t.Fatalf("publish %q %v", serial, err)
		}
	}
	if a.attaches != 1 {
		t.Fatal("retry wrote twice")
	}
	if err := s.DeleteVolume(ctx, id); !errors.Is(err, domain.ErrVolumeInUse) || storage.deletions != 0 {
		t.Fatalf("attached disk deletion: %v", err)
	}
	for range 2 {
		if err := s.Unpublish(ctx, id, uuid.String()); err != nil {
			t.Fatal(err)
		}
	}
	if a.detaches != 1 {
		t.Fatal("detach retry wrote twice")
	}
	if err := s.DeleteVolume(ctx, id); err != nil || storage.deletions != 1 {
		t.Fatalf("delete %v", err)
	}
}
func TestUnsafeReferencesNeverMutate(t *testing.T) {
	for _, state := range []domain.ReferenceState{domain.ReferenceAttached, domain.ReferencePending, domain.ReferenceSnapshot, domain.ReferenceUnused} {
		t.Run(string(rune('A'+state)), func(t *testing.T) {
			s, storage, req, id := volumeFixture(t)
			uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
			vm := domain.VirtualMachine{Node: "pve", VMID: 100, ID: uuid}
			a := &attachmentStub{machines: []domain.VirtualMachine{vm}, refs: domain.References{{Source: vm.ReferenceSource(), Slot: "scsi0", Serial: "wrong", State: state}}}
			s.attachments = a
			_, _ = s.EnsureVolume(context.Background(), req)
			if _, err := s.Publish(context.Background(), id, uuid.String()); !errors.Is(err, domain.ErrVolumeInUse) {
				t.Fatal(err)
			}
			if err := s.Unpublish(context.Background(), id, uuid.String()); !errors.Is(err, domain.ErrAttachmentConflict) {
				t.Fatal(err)
			}
			if err := s.DeleteVolume(context.Background(), id); !errors.Is(err, domain.ErrVolumeInUse) {
				t.Fatal(err)
			}
			if a.attaches+a.detaches+storage.deletions != 0 {
				t.Fatal("unsafe reference mutated")
			}
		})
	}
}
func TestAttachmentUnknownOutcome(t *testing.T) {
	s, _, req, id := volumeFixture(t)
	uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	a := &attachmentStub{machines: []domain.VirtualMachine{{Node: "pve", VMID: 100, ID: uuid}}, writeErr: ErrAttachmentUnconfirmed}
	s.attachments = a
	_, _ = s.EnsureVolume(context.Background(), req)
	if _, err := s.Publish(context.Background(), id, uuid.String()); !errors.Is(err, ErrAttachmentUnconfirmed) {
		t.Fatal(err)
	}
	if a.attaches != 1 {
		t.Fatal("write retried")
	}
	a.readErr = errors.New("inventory unavailable")
	if err := s.DeleteVolume(context.Background(), id); !errors.Is(err, a.readErr) {
		t.Fatal(err)
	}
}
