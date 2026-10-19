package domain

import (
	"errors"
	"testing"
)

func attachmentFixture(t *testing.T) (Identity, VirtualMachine, DiskReference) {
	t.Helper()
	req, err := NewRequest("disk", Placement{Node: "pve", Storage: "pool"}, 7777, GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParseHandle(req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	uuid, err := ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	if err != nil {
		t.Fatal(err)
	}
	vm := VirtualMachine{Node: "pve", VMID: 100, ID: uuid}
	return id, vm, DiskReference{Source: vm.ReferenceSource(), Slot: "scsi1", Serial: id.Serial(), State: ReferenceAttached}
}

func TestAttachmentRetryAndNodeSelection(t *testing.T) {
	id, vm, ref := attachmentFixture(t)
	empty := References{}
	if attached, err := empty.CheckAttach(id, vm); attached || err != nil {
		t.Fatalf("new attachment rejected: %v %v", attached, err)
	}
	if _, found, err := empty.DetachTarget(id, vm.ID); found || err != nil || !empty.DetachedFrom(vm.ID) || empty.CheckDelete() != nil {
		t.Fatalf("absence is not idempotent: %v", err)
	}
	refs := References{ref}
	if got, err := ref.DetachableVM(id); err != nil || got != vm {
		t.Fatalf("valid reference rejected: %+v %v", got, err)
	}
	if attached, err := refs.CheckAttach(id, vm); !attached || err != nil {
		t.Fatalf("retry requires another write: %v", err)
	}
	for _, node := range []NodeID{vm.ID, {}} {
		if got, found, err := refs.DetachTarget(id, node); got != ref || !found || err != nil || refs.DetachedFrom(node) {
			t.Fatalf("managed attachment not selected: %+v %v", got, err)
		}
	}
	other, _ := ParseNodeID("38f7a2bf-c484-47c9-90f6-250caaf28986")
	if _, found, err := refs.DetachTarget(id, other); found || err != nil || !refs.DetachedFrom(other) {
		t.Fatalf("unpublish for another node selected this disk: %v", err)
	}
}

func TestUnsafeReferencesBlockAttachmentDetachmentAndDeletion(t *testing.T) {
	id, vm, original := attachmentFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*DiskReference)
	}{
		{"pending", func(r *DiskReference) { r.State = ReferencePending }},
		{"snapshot", func(r *DiskReference) { r.State = ReferenceSnapshot }},
		{"unused", func(r *DiskReference) { r.State = ReferenceUnused }},
		{"invalid state", func(r *DiskReference) { r.State = ReferenceState(99) }},
		{"foreign serial", func(r *DiskReference) { r.Serial = "other" }},
		{"template with UUID", func(r *DiskReference) { r.Source.Kind = SourceTemplate }},
		{"container with UUID", func(r *DiskReference) { r.Source.Kind = SourceContainer }},
		{"unknown source", func(r *DiskReference) { r.Source.Kind = 0 }},
		{"missing UUID", func(r *DiskReference) { r.Source.NodeID = NodeID{} }},
		{"reserved owner", func(r *DiskReference) { r.Source.VMID = id.OwnerVMID() }},
		{"wrong host", func(r *DiskReference) { r.Source.Node = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := original
			tc.change(&ref)
			refs := References{ref}
			if got, err := ref.DetachableVM(id); got != (VirtualMachine{}) || !errors.Is(err, ErrAttachmentConflict) {
				t.Fatalf("unsafe reference returned a VM: %+v %v", got, err)
			}
			if _, err := refs.CheckAttach(id, vm); !errors.Is(err, ErrVolumeInUse) {
				t.Fatalf("unsafe reference accepted for attachment: %v", err)
			}
			if _, found, err := refs.DetachTarget(id, NodeID{}); found || !errors.Is(err, ErrAttachmentConflict) {
				t.Fatalf("unsafe reference accepted for detachment: %v", err)
			}
			if refs.DetachedFrom(NodeID{}) || !errors.Is(refs.CheckDelete(), ErrVolumeInUse) {
				t.Fatal("reference ignored for deletion")
			}
		})
	}
	refs := References{original, original}
	if _, err := refs.CheckAttach(id, vm); !errors.Is(err, ErrVolumeInUse) {
		t.Fatal("multiple references accepted", err)
	}
	if _, found, err := refs.DetachTarget(id, vm.ID); found || !errors.Is(err, ErrAttachmentConflict) {
		t.Fatal("ambiguous reference selected", err)
	}
}

func TestAttachmentTargetCannotUseReservedOwnerOrInvalidVM(t *testing.T) {
	id, vm, _ := attachmentFixture(t)
	for _, tc := range []struct {
		vm   VirtualMachine
		want error
	}{
		{VirtualMachine{}, ErrInvalidInventory},
		{VirtualMachine{Node: "pve", VMID: id.OwnerVMID(), ID: vm.ID}, ErrOwnerInUse},
		{VirtualMachine{Node: "other", VMID: vm.VMID, ID: vm.ID}, ErrAttachmentConflict},
	} {
		if _, err := (References{}).CheckAttach(id, tc.vm); !errors.Is(err, tc.want) {
			t.Fatalf("unsafe target accepted: %+v %v", tc.vm, err)
		}
	}
}
