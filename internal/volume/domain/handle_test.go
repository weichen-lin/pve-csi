package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseHandlePreservesV1Identity(t *testing.T) {
	const filename = "vm-482-csi-c6e1db68bf83f511115e68f7c6422051f02d8ba0a4f304894a0b28f9558834d2"
	const handle = "v1:test-pve:test-pool:" + filename
	id, err := ParseHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	if id.ID() != handle || id.Reference() != "test-pool:"+filename || id.OwnerVMID() != 482 || id.Placement() != (Placement{Node: "test-pve", Storage: "test-pool"}) {
		t.Fatalf("identity was changed: %+v", id)
	}
	req := sampleRequest()
	req.Name = ProvisioningName("abc")
	if req.Handle() != id.ID() {
		t.Fatal("parser no longer accepts the CreateVolume identity")
	}
}

func TestParseHandleRejectsInvalidIdentity(t *testing.T) {
	valid := sampleRequest().Handle()
	for _, raw := range []string{
		"", " " + valid, valid + " ", valid + ":extra",
		strings.Replace(valid, "v1:", "v2:", 1),
		strings.Replace(valid, "test-pve", "../pve", 1),
		strings.Replace(valid, "test-pool", "pool?x=1", 1),
		strings.Replace(valid, "vm-482-", "vm-0482-", 1),
		strings.Replace(valid, "vm-482-", "vm-99-", 1),
		strings.Replace(valid, "vm-482-", "vm-1000000000-", 1),
		strings.Replace(valid, "test-pve", strings.Repeat("n", 63), 1),
		valid[:len(valid)-1], strings.ToUpper(valid),
		"v1:test-pve:test-pool:vm-482-disk-0",
		"v1:test-pve:test-pool:vm-482-cloudinit",
		strings.Replace(valid, "vm-482", "vm%2D482", 1),
		strings.Replace(valid, "vm-482", "vm-482\n", 1),
	} {
		if _, err := ParseHandle(raw); !errors.Is(err, ErrInvalidHandle) {
			t.Errorf("accepted malformed handle: %q, %v", raw, err)
		}
	}
	if err := (Identity{}).Validate(); !errors.Is(err, ErrInvalidHandle) {
		t.Fatal("zero identity was accepted")
	}
}

func TestRestoreVolumeFromHandle(t *testing.T) {
	req := sampleRequest()
	id, err := ParseHandle(req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	observed := sampleAllocation(req)
	// An already compatible larger allocation need not be whole-GiB aligned.
	observed.CapacityBytes = 3*GiB + 1
	volume, err := Restore(id, observed)
	if err != nil || volume.ID() != req.Handle() || volume.CapacityBytes() != observed.CapacityBytes {
		t.Fatalf("restore = %+v, %v", volume, err)
	}
	for _, change := range []func(*Allocation){
		func(a *Allocation) { a.Reference = "different" },
		func(a *Allocation) { a.Placement.Node = "other" },
		func(a *Allocation) { a.Placement.Storage = "other" },
		func(a *Allocation) { a.OwnerVMID++ },
		func(a *Allocation) { a.CapacityBytes = 0 },
		func(a *Allocation) { a.CapacityBytes = -1 },
		func(a *Allocation) { a.CapacityBytes = GiB - 1 },
	} {
		bad := observed
		change(&bad)
		if _, err := Restore(id, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid observation accepted: %v", err)
		}
	}
	if _, err := Restore(Identity{}, observed); !errors.Is(err, ErrInvalidHandle) {
		t.Fatal("restore accepted zero identity")
	}
}
