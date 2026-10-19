package domain

import (
	"errors"
	"math"
	"unicode"
	"unicode/utf8"
)

// Request is a command payload, not a confirmed Volume. Validate it before IO.
// OwnerVMID is allocation ownership, not an attachment target.
// The operator must reserve it and keep it stable across retries: PVE uses the
// owner for permissions and VM disk cleanup.
type Request struct {
	name               string
	Node               string
	Storage            string
	OwnerVMID          int
	CapacityBytes      int64
	CapacityLimitBytes int64
}

// NewRequest accepts the original name, never a pre-hashed backend name.
// Identity derivation belongs here so all callers use the same retry key.
func NewRequest(name string, placement Placement, owner int, capacity, limit int64) (Request, error) {
	r := Request{name: name, Node: placement.Node, Storage: placement.Storage, OwnerVMID: owner, CapacityBytes: capacity, CapacityLimitBytes: limit}
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	return r, nil
}

type Placement struct {
	Node    string
	Storage string
}

// Allocation is a backend observation. Confirm or Restore validates it before
// constructing a Volume.
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
func (v Volume) Placement() Placement  { return v.allocation.Placement }
func (v Volume) CapacityBytes() int64  { return v.allocation.CapacityBytes }
func (r Request) Placement() Placement { return Placement{Node: r.Node, Storage: r.Storage} }

func (r Request) CapacityLimit() int64 {
	if r.CapacityLimitBytes == 0 {
		return r.CapacityBytes
	}
	return r.CapacityLimitBytes
}

func (r Request) Validate() error {
	if r.name == "" || len(r.name) > 128 || !utf8.ValidString(r.name) {
		return ErrInvalidName
	}
	for _, r := range r.name {
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			return ErrInvalidName
		}
	}
	if err := (Scope{Node: r.Node, OwnerVMID: r.OwnerVMID}).Validate(); err != nil {
		return err
	}
	if !ValidStorageID(r.Storage) {
		return errors.New("volume allocation requires a valid PVE storage ID")
	}
	if _, err := ParseHandle(r.Handle()); err != nil {
		return err
	}
	if r.CapacityBytes <= 0 || r.CapacityBytes%GiB != 0 {
		return errors.New("volume capacity must be a positive whole number of GiB")
	}
	if r.CapacityLimitBytes < 0 || (r.CapacityLimitBytes != 0 && r.CapacityLimitBytes < r.CapacityBytes) {
		return errors.New("volume capacity limit must be zero or at least the requested capacity")
	}
	return nil
}

const GiB int64 = 1 << 30

// NormalizeCapacity converts an optional capacity range into whole-GiB
// allocation bounds. specified distinguishes an omitted range from empty input.
func NormalizeCapacity(required, upper int64, specified bool) (int64, int64, error) {
	limit := int64(math.MaxInt64)
	if specified {
		if required < 0 || upper < 0 || (required == 0 && upper == 0) || (upper > 0 && required > upper) {
			return 0, 0, ErrInvalidCapacity
		}
		if upper != 0 {
			limit = upper
		}
	} else {
		required = 0
	}
	if required < GiB {
		required = GiB
	}
	if remainder := required % GiB; remainder != 0 {
		increment := GiB - remainder
		if required > math.MaxInt64-increment {
			return 0, 0, ErrCapacityOverflow
		}
		required += increment
	}
	if required > limit {
		return 0, 0, ErrCapacityOutOfRange
	}
	return required, limit, nil
}

var (
	ErrInvalidName        = errors.New("volume name must be 1 to 128 bytes of valid UTF-8 without prohibited control characters")
	ErrCapacity           = errors.New("requested volume exceeds storage available capacity")
	ErrStorage            = errors.New("unsupported volume storage")
	ErrPoolDisabled       = errors.New("storage pool is disabled")
	ErrPoolInactive       = errors.New("storage pool is inactive")
	ErrInvalidHandle      = errors.New("invalid v1 volume ID")
	ErrConflict           = errors.New("existing volume conflicts with requested owner, format, content or capacity")
	ErrInvalidCapacity    = errors.New("capacity range must contain positive bounds in ascending order")
	ErrCapacityOutOfRange = errors.New("capacity range cannot fit a whole-GiB volume of at least 1 GiB")
	ErrCapacityOverflow   = errors.New("capacity cannot be rounded to whole GiB without overflow")
)
