package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	nodePattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)
	storagePattern = regexp.MustCompile(`(?i)^[a-z][a-z0-9_.-]*[a-z0-9]$`)
)

func ValidNodeName(value string) bool {
	return len(value) <= 63 && nodePattern.MatchString(value) && !strings.HasSuffix(value, "-")
}

func ValidStorageID(value string) bool { return storagePattern.MatchString(value) }

func ValidVMID(value int) bool { return value >= 100 && value <= 999999999 }

// NameDigest is safe for logging without revealing the original request name.
// It is also the internal v1 namespace used when deriving a disk identity.
func NameDigest(original string) string {
	digest := sha256.Sum256([]byte(original))
	return fmt.Sprintf("csi-v1-%x", digest)
}

func (r Request) Filename() string {
	// Preserve the established v1 identity, including its two hashing steps.
	digest := sha256.Sum256([]byte(NameDigest(r.name)))
	return fmt.Sprintf("vm-%d-csi-%x", r.OwnerVMID, digest)
}

func (r Request) AllocationReference() string { return r.Storage + ":" + r.Filename() }

func (r Request) Handle() string {
	return "v1:" + r.Node + ":" + r.Storage + ":" + r.Filename()
}

// Scope identifies the node and reserved disk owner managed by one service.
type Scope struct {
	Node      string
	OwnerVMID int
}

func (s Scope) Contains(placement Placement, owner int) bool {
	return placement.Node == s.Node && owner == s.OwnerVMID
}

func (s Scope) Validate() error {
	if !ValidNodeName(s.Node) {
		return errors.New("invalid PVE node name")
	}
	if !ValidVMID(s.OwnerVMID) {
		return errors.New("owner VMID must be from 100 to 999999999")
	}
	// v1:<node>:<storage>:vm-<owner>-csi-<sha256>; shortest storage ID is 2 bytes.
	if len("v1:::")+len(s.Node)+2+len("vm--csi-")+len(strconv.Itoa(s.OwnerVMID))+64 > 128 {
		return errors.New("node and owner cannot produce a volume handle within 128 bytes")
	}
	return nil
}

var filenamePattern = regexp.MustCompile(`^vm-([1-9][0-9]*)-csi-[0-9a-f]{64}$`)

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
	if err != nil || !ValidVMID(owner) {
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

// Restore validates an existing allocation without a provisioning request.
// Observed capacity need not be a whole number of GiB.
func Restore(id Identity, observed Allocation) (Volume, error) {
	if err := id.Validate(); err != nil {
		return Volume{}, err
	}
	if observed.Reference != id.Reference() || observed.Placement != id.Placement() || observed.OwnerVMID != id.OwnerVMID() || observed.CapacityBytes < GiB {
		return Volume{}, ErrConflict
	}
	return Volume{id: id.ID(), allocation: observed}, nil
}
