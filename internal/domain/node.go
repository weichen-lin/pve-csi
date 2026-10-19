package domain

import (
	"errors"
	"regexp"
	"strings"
)

var (
	ErrInvalidNodeID    = errors.New("node ID must be a nonzero UUID")
	ErrNodeNotFound     = errors.New("no VM matches the node UUID")
	ErrNodeAmbiguous    = errors.New("multiple VMs share the node UUID")
	ErrInvalidInventory = errors.New("VM inventory is invalid or incomplete")
	uuidPattern         = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// NodeID identifies a guest independently of its name, IP address or VMID.
type NodeID struct{ uuid string }

func ParseNodeID(raw string) (NodeID, error) {
	if !uuidPattern.MatchString(raw) || raw == "00000000-0000-0000-0000-000000000000" || strings.EqualFold(raw, "ffffffff-ffff-ffff-ffff-ffffffffffff") {
		return NodeID{}, ErrInvalidNodeID
	}
	return NodeID{uuid: strings.ToLower(raw)}, nil
}

func (id NodeID) String() string { return id.uuid }

// VirtualMachine is a non-template VM candidate with a guest UUID.
// References from containers/templates are represented by ReferenceSource.
type VirtualMachine struct {
	Node string
	VMID int
	ID   NodeID
}

func (vm VirtualMachine) Validate() error {
	if !ValidNodeName(vm.Node) || !ValidVMID(vm.VMID) || vm.ID == (NodeID{}) {
		return ErrInvalidInventory
	}
	return nil
}

// MatchNode requires a complete observation. A first match alone is not proof
// of uniqueness; clones can retain the original guest's UUID.
func MatchNode(node string, id NodeID, machines []VirtualMachine) (VirtualMachine, error) {
	if id.uuid == "" {
		return VirtualMachine{}, ErrInvalidNodeID
	}
	if !ValidNodeName(node) {
		return VirtualMachine{}, ErrInvalidInventory
	}
	seen := make(map[int]bool, len(machines))
	var match VirtualMachine
	count := 0
	for _, vm := range machines {
		if vm.Node != node || vm.Validate() != nil || seen[vm.VMID] {
			return VirtualMachine{}, ErrInvalidInventory
		}
		seen[vm.VMID] = true
		if vm.ID == id {
			match = vm
			count++
		}
	}
	switch count {
	case 0:
		return VirtualMachine{}, ErrNodeNotFound
	case 1:
		return match, nil
	default:
		return VirtualMachine{}, ErrNodeAmbiguous
	}
}
