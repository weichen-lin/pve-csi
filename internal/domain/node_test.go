package domain

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestVMIDBoundaries(t *testing.T) {
	id, _ := ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	for _, tc := range []struct {
		vmid  int
		valid bool
	}{
		{-1, false}, {0, false}, {99, false}, {100, true}, {7777, true}, {999999999, true}, {1000000000, false},
	} {
		ownerErr := (Scope{Node: "pve", OwnerVMID: tc.vmid}).Validate()
		vmErr := (VirtualMachine{Node: "pve", VMID: tc.vmid, ID: id}).Validate()
		_, handleErr := ParseHandle("v1:pve:pool:vm-" + strconv.Itoa(tc.vmid) + "-csi-" + strings.Repeat("a", 64))
		if (ownerErr == nil) != tc.valid || (vmErr == nil) != tc.valid || (handleErr == nil) != tc.valid {
			t.Errorf("VMID %d: owner=%v VM=%v handle=%v; want valid=%t", tc.vmid, ownerErr, vmErr, handleErr, tc.valid)
		}
	}
	for _, raw := range []string{"0100", "+100", "100.0", "99999999999999999999"} {
		if _, err := ParseHandle("v1:pve:pool:vm-" + raw + "-csi-" + strings.Repeat("a", 64)); !errors.Is(err, ErrInvalidHandle) {
			t.Errorf("accepted invalid handle owner %q: %v", raw, err)
		}
	}
}

func TestNodeIdentity(t *testing.T) {
	const uuid = "153a7c9e-588b-483f-884a-4eba01bc160a"
	id, err := ParseNodeID(strings.ToUpper(uuid))
	if err != nil || id.String() != uuid {
		t.Fatalf("normalization = %v, %v", id, err)
	}
	for _, raw := range []string{"", "100", "00000000-0000-0000-0000-000000000000", "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF", " " + uuid, uuid + "\n", strings.ReplaceAll(uuid, "-", "")} {
		if _, err := ParseNodeID(raw); !errors.Is(err, ErrInvalidNodeID) {
			t.Errorf("accepted invalid node ID %q", raw)
		}
	}
	other, _ := ParseNodeID("38f7a2bf-c484-47c9-90f6-250caaf28986")
	vm := VirtualMachine{Node: "pve", VMID: 100, ID: id}
	for _, tt := range []struct {
		name string
		vms  []VirtualMachine
		want error
	}{
		{"unique", []VirtualMachine{vm, {Node: "pve", VMID: 101, ID: other}}, nil},
		{"empty", nil, ErrNodeNotFound},
		{"no match", []VirtualMachine{{Node: "pve", VMID: 101, ID: other}}, ErrNodeNotFound},
		{"cloned UUID", []VirtualMachine{vm, {Node: "pve", VMID: 101, ID: id}}, ErrNodeAmbiguous},
		{"duplicate entry", []VirtualMachine{vm, vm}, ErrInvalidInventory},
		{"wrong host", []VirtualMachine{{Node: "other", VMID: 100, ID: id}}, ErrInvalidInventory},
		{"missing UUID after match", []VirtualMachine{vm, {Node: "pve", VMID: 101}}, ErrInvalidInventory},
		{"invalid VMID", []VirtualMachine{{Node: "pve", VMID: 0, ID: id}}, ErrInvalidInventory},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MatchNode("pve", id, tt.vms)
			if !errors.Is(err, tt.want) || (err == nil && got != vm) || (err != nil && got != (VirtualMachine{})) {
				t.Fatalf("MatchNode = %v, %v; want error %v", got, err, tt.want)
			}
		})
	}
}
