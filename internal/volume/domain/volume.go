package domain

import (
	"errors"
	"strings"
	"unicode"
)

// Request is a command payload, not a confirmed Volume. Validate it before IO.
// OwnerVMID is allocation ownership, never an attachment target or query VM.
// The operator must reserve it and keep it stable across retries: PVE uses the
// owner for permissions and VM disk cleanup.
type Request struct {
	Name               string
	Node               string
	Storage            string
	OwnerVMID          int
	CapacityBytes      int64
	CapacityLimitBytes int64
}

type Placement struct {
	Node    string
	Storage string
}

// Allocation is an observation supplied by a gateway. Only Confirm can turn it
// into a Volume; untrusted metadata must not bypass the compatibility checks.
type Allocation struct {
	Reference     string
	Placement     Placement
	OwnerVMID     int
	CapacityBytes int64
}

// Volume is an immutable confirmed allocation. It represents observed backend
// state, not a local database record or an atomic remote transaction.
type Volume struct {
	id         string
	allocation Allocation
}

func Confirm(req Request, observed Allocation) (Volume, error) {
	if err := req.Validate(); err != nil {
		return Volume{}, err
	}
	if observed.Reference != req.AllocationReference() || observed.Placement != req.Placement() ||
		observed.OwnerVMID != req.OwnerVMID || observed.CapacityBytes < req.CapacityBytes || observed.CapacityBytes > req.CapacityLimit() {
		return Volume{}, ErrConflict
	}
	return Volume{id: req.Handle(), allocation: observed}, nil
}

func (v Volume) ID() string            { return v.id }
func (v Volume) Reference() string     { return v.allocation.Reference }
func (v Volume) Placement() Placement  { return v.allocation.Placement }
func (v Volume) OwnerVMID() int        { return v.allocation.OwnerVMID }
func (v Volume) CapacityBytes() int64  { return v.allocation.CapacityBytes }
func (r Request) Placement() Placement { return Placement{Node: r.Node, Storage: r.Storage} }

func (v Volume) CompatibleWith(req Request) bool {
	confirmed, err := Confirm(req, v.allocation)
	return err == nil && v.id == confirmed.id
}

func (r Request) CapacityLimit() int64 {
	if r.CapacityLimitBytes == 0 {
		return r.CapacityBytes
	}
	return r.CapacityLimitBytes
}

func (r Request) Validate() error {
	if r.Name == "" || len(r.Name) > 128 || strings.TrimSpace(r.Name) != r.Name || strings.ContainsFunc(r.Name, unicode.IsControl) {
		return errors.New("volume name must be 1 to 128 bytes without control characters or surrounding whitespace")
	}
	if !ValidNodeName(r.Node) {
		return errors.New("volume allocation requires a valid PVE node name")
	}
	if !ValidStorageID(r.Storage) {
		return errors.New("volume allocation requires a valid PVE storage ID")
	}
	if r.OwnerVMID < 100 || r.OwnerVMID > 999999999 {
		return errors.New("volume owner VMID must be from 100 to 999999999")
	}
	if r.CapacityBytes <= 0 || r.CapacityBytes%GiB != 0 {
		return errors.New("volume capacity must be a positive whole number of GiB")
	}
	if r.CapacityLimitBytes < 0 || (r.CapacityLimitBytes != 0 && r.CapacityLimitBytes < r.CapacityBytes) {
		return errors.New("volume capacity limit must be zero or at least the requested capacity")
	}
	return nil
}
