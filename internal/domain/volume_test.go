package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestRequestPreservesOriginalNameAndExistingIdentity(t *testing.T) {
	for _, tc := range []struct{ name, handle string }{
		{"pvc-one", "v1:pve:pool:vm-7777-csi-547c46c7922f79054968215530dffdd76e0b179286f8f56ed5262d80f042a6e9"},
		{" one\t\n\r", "v1:pve:pool:vm-7777-csi-6fa8c023b140b5ab6a1bbc6a550112c2262a9ea1b13633285fa3edc682676891"},
		{"one", "v1:pve:pool:vm-7777-csi-75b56d42c65cfd755d43fca07101daba2d91c6ae58c09150da9eec4677166b28"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 2 {
				req, err := NewRequest(tc.name, Placement{Node: "pve", Storage: "pool"}, 7777, GiB, 0)
				if err != nil || req.Handle() != tc.handle {
					t.Fatalf("identity changed: %q %v", req.Handle(), err)
				}
			}
		})
	}
	for _, name := range []string{"", strings.Repeat("a", 129), "bad\x00name", "\xff"} {
		if _, err := NewRequest(name, Placement{Node: "pve", Storage: "pool"}, 7777, GiB, 0); !errors.Is(err, ErrInvalidName) {
			t.Errorf("invalid name %q accepted: %v", name, err)
		}
	}
}

func TestCapacityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name            string
		required, upper int64
		specified       bool
		want, limit     int64
		err             error
	}{
		{"default", 0, 0, false, GiB, math.MaxInt64, nil},
		{"empty range", 0, 0, true, 0, 0, ErrInvalidCapacity},
		{"one byte rounds", 1, 0, true, GiB, math.MaxInt64, nil},
		{"upper only", 0, GiB, true, GiB, GiB, nil},
		{"too small", 0, GiB - 1, true, 0, 0, ErrCapacityOutOfRange},
		{"round to two", GiB + 1, 2 * GiB, true, 2 * GiB, 2 * GiB, nil},
		{"negative", -1, 0, true, 0, 0, ErrInvalidCapacity},
		{"overflow", math.MaxInt64, 0, true, 0, 0, ErrCapacityOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, limit, err := NormalizeCapacity(tc.required, tc.upper, tc.specified)
			if got != tc.want || limit != tc.limit || !errors.Is(err, tc.err) {
				t.Fatalf("capacity=%d limit=%d err=%v", got, limit, err)
			}
		})
	}
}

func TestConfirmationRejectsMismatchedDisk(t *testing.T) {
	req, err := NewRequest("one", Placement{Node: "pve", Storage: "pool"}, 7777, GiB, 2*GiB)
	if err != nil {
		t.Fatal(err)
	}
	base := Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: GiB}
	for _, change := range []func(*Allocation){
		func(a *Allocation) { a.Reference = "pool:unrelated" },
		func(a *Allocation) { a.OwnerVMID = 100 },
		func(a *Allocation) { a.Placement.Node = "other" },
		func(a *Allocation) { a.CapacityBytes = GiB - 1 },
		func(a *Allocation) { a.CapacityBytes = 2*GiB + 1 },
	} {
		a := base
		change(&a)
		if _, err := Confirm(req, a); !errors.Is(err, ErrConflict) {
			t.Fatalf("confirmed wrong disk: %+v %v", a, err)
		}
	}
	for _, capacity := range []int64{GiB, 2 * GiB} {
		a := base
		a.CapacityBytes = capacity
		if _, err := Confirm(req, a); err != nil {
			t.Fatal(err)
		}
	}
}
