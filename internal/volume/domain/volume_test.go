package domain

import (
	"errors"
	"math"
	"testing"
)

func sampleRequest() Request {
	return Request{Name: "abc", Node: "test-pve", Storage: "test-pool", OwnerVMID: 482, CapacityBytes: 2 * GiB}
}

func sampleAllocation(req Request) Allocation {
	return Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes}
}

func TestVolumeConfirmationAndImmutableState(t *testing.T) {
	req := sampleRequest()
	observed := sampleAllocation(req)
	volume, err := Confirm(req, observed)
	if err != nil {
		t.Fatal(err)
	}
	observed.OwnerVMID = 101
	observed.Placement.Node = "other-pve"
	placement := volume.Placement()
	placement.Storage = "other-pool"
	if !volume.CompatibleWith(req) || volume.OwnerVMID() != 482 || volume.Placement() != req.Placement() || volume.Placement() == placement {
		t.Fatal("caller mutation changed confirmed volume state")
	}
	if (Volume{}).CompatibleWith(req) {
		t.Fatal("zero Volume is not a confirmed allocation")
	}
	for _, change := range []func(*Allocation){
		func(a *Allocation) { a.Reference = "other" },
		func(a *Allocation) { a.Placement.Node = "other" },
		func(a *Allocation) { a.Placement.Storage = "other" },
		func(a *Allocation) { a.OwnerVMID = 101 },
		func(a *Allocation) { a.CapacityBytes = GiB },
		func(a *Allocation) { a.CapacityBytes = 3 * GiB },
	} {
		allocation := sampleAllocation(req)
		change(&allocation)
		if _, err := Confirm(req, allocation); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid observation accepted: %v", err)
		}
	}
}

func TestVolumeCapacityCompatibility(t *testing.T) {
	req := sampleRequest()
	observed := sampleAllocation(req)
	observed.CapacityBytes = 3 * GiB
	if _, err := Confirm(req, observed); !errors.Is(err, ErrConflict) {
		t.Fatal("zero limit must keep exact-size behavior")
	}
	req.CapacityLimitBytes = 4 * GiB
	volume, err := Confirm(req, observed)
	if err != nil || volume.CapacityBytes() != 3*GiB {
		t.Fatalf("compatible larger allocation = %v, %v", volume, err)
	}
	req.CapacityLimitBytes = 2 * GiB
	if volume.CompatibleWith(req) {
		t.Fatal("allocation above the new upper limit accepted")
	}
}

func TestIdentityV1Vectors(t *testing.T) {
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if ProvisioningName("abc") != "csi-v1-"+abc {
		t.Fatal("first hash changed")
	}
	req := sampleRequest()
	if req.Filename() != "vm-482-csi-"+abc {
		t.Fatal("backend filename changed")
	}
	if req.Handle() != "v1:test-pve:test-pool:vm-482-csi-"+abc {
		t.Fatal("v1 handle changed")
	}
	// Fixed independently from SHA-256("csi-v1-" + SHA-256("abc")).
	const doubleHash = "c6e1db68bf83f511115e68f7c6422051f02d8ba0a4f304894a0b28f9558834d2"
	req.Name = ProvisioningName("abc")
	if req.Filename() != "vm-482-csi-"+doubleHash {
		t.Fatal("CSI double hash changed")
	}
	if req.AllocationReference() != "test-pool:vm-482-csi-"+doubleHash {
		t.Fatal("allocation reference changed")
	}
	if req.Handle() != "v1:test-pve:test-pool:vm-482-csi-"+doubleHash {
		t.Fatal("CSI v1 handle changed")
	}
	if ProvisioningName("abc") == ProvisioningName(" abc") || ProvisioningName("abc") == ProvisioningName("abc ") {
		t.Fatal("name bytes were normalized")
	}
}

func TestDomainCapacityRange(t *testing.T) {
	for _, tc := range []struct {
		name            string
		required, limit int64
		specified       bool
		want            int64
		err             error
	}{
		{"omitted", 0, 0, false, GiB, nil},
		{"round up", GiB + 1, 3 * GiB, true, 2 * GiB, nil},
		{"explicit empty", 0, 0, true, 0, ErrInvalidCapacity},
		{"inverted", 3 * GiB, 2 * GiB, true, 0, ErrInvalidCapacity},
		{"too tight", GiB + 1, GiB + 1, true, 0, ErrCapacityOutOfRange},
		{"overflow", math.MaxInt64, 0, true, 0, ErrCapacityOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := NormalizeCapacity(tc.required, tc.limit, tc.specified)
			if got != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("capacity = %d, %v; want %d, %v", got, err, tc.want, tc.err)
			}
		})
	}
}
