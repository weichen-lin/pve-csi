package domain

import (
	"regexp"
	"strconv"
	"strings"
)

var filenamePattern = regexp.MustCompile(`^vm-([1-9][0-9]{2,8})-csi-[0-9a-f]{64}$`)

// Identity is the canonical v1 locator, not proof of backend ownership.
// The original provisioning name cannot be recovered from its digest.
type Identity struct {
	id        string
	placement Placement
	owner     int
	filename  string
}

func ParseHandle(raw string) (Identity, error) {
	if len(raw) == 0 || len(raw) > 128 {
		return Identity{}, ErrInvalidHandle
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 4 || parts[0] != "v1" || !ValidNodeName(parts[1]) || !ValidStorageID(parts[2]) {
		return Identity{}, ErrInvalidHandle
	}
	match := filenamePattern.FindStringSubmatch(parts[3])
	if match == nil {
		return Identity{}, ErrInvalidHandle
	}
	owner, err := strconv.Atoi(match[1])
	if err != nil {
		return Identity{}, ErrInvalidHandle
	}
	return Identity{id: raw, placement: Placement{Node: parts[1], Storage: parts[2]}, owner: owner, filename: parts[3]}, nil
}

func (id Identity) ID() string           { return id.id }
func (id Identity) Placement() Placement { return id.placement }
func (id Identity) OwnerVMID() int       { return id.owner }
func (id Identity) Reference() string    { return id.placement.Storage + ":" + id.filename }

func (id Identity) Validate() error {
	parsed, err := ParseHandle(id.id)
	if err != nil || parsed != id {
		return ErrInvalidHandle
	}
	return nil
}

// Restore validates a read-only observation without inventing a creation
// request. Capacity may be larger than a whole-GiB allocation request, as in
// Confirm's existing capacity-range contract. Format checks belong to adapters.
func Restore(id Identity, observed Allocation) (Volume, error) {
	if err := id.Validate(); err != nil {
		return Volume{}, err
	}
	if observed.Reference != id.Reference() || observed.Placement != id.Placement() || observed.OwnerVMID != id.OwnerVMID() || observed.CapacityBytes < GiB {
		return Volume{}, ErrConflict
	}
	return Volume{id: id.ID(), allocation: observed}, nil
}
