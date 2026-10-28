package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

var (
	ErrVolumeInUse        = errors.New("volume has an existing VM, pending or snapshot reference")
	ErrAttachmentConflict = errors.New("attachment identity does not match the managed volume")
	ErrOwnerInUse         = errors.New("reserved disk owner VMID is used by a VM or container")
)

// Serial is independent of VMID, SCSI slot and Linux device enumeration.
func (id Identity) Serial() string {
	digest := sha256.Sum256([]byte(id.ID()))
	return fmt.Sprintf("csi%x", digest[:14])
}

type ReferenceState int

const (
	ReferenceAttached ReferenceState = iota
	ReferencePending
	ReferenceSnapshot
	ReferenceUnused
)

type DiskReference struct {
	Source ReferenceSource
	Slot   string
	Serial string
	State  ReferenceState
}

type SourceKind int

const (
	SourceVM SourceKind = iota + 1
	SourceTemplate
	SourceContainer
)

// ReferenceSource may omit NodeID; templates and containers still block deletion.
type ReferenceSource struct {
	Kind   SourceKind
	Node   string
	VMID   int
	NodeID NodeID
}

func (vm VirtualMachine) ReferenceSource() ReferenceSource {
	return ReferenceSource{Kind: SourceVM, Node: vm.Node, VMID: vm.VMID, NodeID: vm.ID}
}

// VirtualMachine rejects containers and templates even if their UUID matches.
func (source ReferenceSource) VirtualMachine() (VirtualMachine, error) {
	vm := VirtualMachine{Node: source.Node, VMID: source.VMID, ID: source.NodeID}
	if source.Kind != SourceVM || vm.Validate() != nil {
		return VirtualMachine{}, ErrAttachmentConflict
	}
	return vm, nil
}

// References is one complete observation, never a partial scan.
type References []DiskReference

// DetachableVM rejects non-live references and mismatched disk identities.
func (ref DiskReference) DetachableVM(id Identity) (VirtualMachine, error) {
	if err := id.Validate(); err != nil {
		return VirtualMachine{}, err
	}
	vm, err := ref.Source.VirtualMachine()
	if err != nil || id.CheckAttachmentTarget(vm) != nil || ref.State != ReferenceAttached || ref.Serial != id.Serial() {
		return VirtualMachine{}, ErrAttachmentConflict
	}
	return vm, nil
}

func (id Identity) CheckAttachmentTarget(vm VirtualMachine) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if err := vm.Validate(); err != nil {
		return err
	}
	if vm.VMID == id.OwnerVMID() {
		return ErrOwnerInUse
	}
	if vm.Node != id.Placement().Node {
		return ErrAttachmentConflict
	}
	return nil
}

// CheckAttach reports whether the requested attachment already exists.
func (refs References) CheckAttach(id Identity, vm VirtualMachine) (bool, error) {
	if err := id.CheckAttachmentTarget(vm); err != nil {
		return false, err
	}
	if len(refs) == 0 {
		return false, nil
	}
	if len(refs) == 1 && refs[0].State == ReferenceAttached && refs[0].Source == vm.ReferenceSource() && refs[0].Serial == id.Serial() {
		return true, nil
	}
	return false, ErrVolumeInUse
}

// DetachTarget selects the sole safe attachment for a node; a zero node ID
// means all nodes. Absence is successful and must not cause a write.
func (refs References) DetachTarget(id Identity, node NodeID) (DiskReference, bool, error) {
	if err := id.Validate(); err != nil {
		return DiskReference{}, false, err
	}
	var target DiskReference
	count := 0
	for _, ref := range refs {
		if node == (NodeID{}) || ref.Source.NodeID == node {
			target = ref
			count++
		}
	}
	if count == 0 {
		return DiskReference{}, false, nil
	}
	if count != 1 {
		return DiskReference{}, false, ErrAttachmentConflict
	}
	if _, err := target.DetachableVM(id); err != nil {
		return DiskReference{}, false, err
	}
	return target, true, nil
}

func (refs References) DetachedFrom(node NodeID) bool {
	for _, ref := range refs {
		if node == (NodeID{}) || ref.Source.NodeID == node {
			return false
		}
	}
	return true
}

func (refs References) CheckDelete() error {
	if len(refs) != 0 {
		return ErrVolumeInUse
	}
	return nil
}
