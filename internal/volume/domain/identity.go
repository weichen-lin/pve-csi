package domain

import (
	"crypto/sha256"
	"fmt"
	"regexp"
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

// ProvisioningName preserves the established v1 namespace and original bytes.
// Filename deliberately applies a second hash; changing either breaks retries.
func ProvisioningName(original string) string {
	digest := sha256.Sum256([]byte(original))
	return fmt.Sprintf("csi-v1-%x", digest)
}

func (r Request) Filename() string {
	digest := sha256.Sum256([]byte(r.Name))
	return fmt.Sprintf("vm-%d-csi-%x", r.OwnerVMID, digest)
}

func (r Request) AllocationReference() string { return r.Storage + ":" + r.Filename() }

func (r Request) Handle() string {
	return "v1:" + r.Node + ":" + r.Storage + ":" + r.Filename()
}
