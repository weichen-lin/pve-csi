package volume

import (
	"context"
	"errors"

	"pve-csi.local/csi-plugin/internal/domain"
)

// CheckFailureDetail is the single source of diagnostic wording for shared
// application failures. Adapters may reuse it when normalizing observations.
// An empty result means the error is unknown; callers must use an authored
// fallback, never err.Error(). Backend-specific hints remain adapter-owned.
func CheckFailureDetail(err error) string {
	if errors.Is(err, context.Canceled) {
		return "check canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "check timed out; check connectivity and PVE service status"
	}
	var failure *CheckFailure
	if errors.As(err, &failure) && failure.Hint() != "" {
		return failure.Hint()
	}
	switch {
	case errors.Is(err, domain.ErrNodeAmbiguous):
		return "multiple visible VMs share this UUID; give each Kubernetes VM a unique UUID"
	case errors.Is(err, domain.ErrNodeNotFound):
		return "selected VM UUID is absent from the visible inventory; check visibility and VM identity"
	case errors.Is(err, domain.ErrInvalidInventory):
		return "VM inventory/configuration is invalid, incomplete or not a non-template VM; check UUIDs, VM type and read permissions"
	case errors.Is(err, domain.ErrAttachmentConflict):
		return "VM identity or configuration changed during the check; verify the target VM and retry"
	case errors.Is(err, domain.ErrOwnerInUse):
		return "reserved disk owner VMID is used by a VM or container; verify the configured owner and resolve the collision before running CSI"
	case errors.Is(err, domain.ErrPoolDisabled):
		return "storage is disabled"
	case errors.Is(err, domain.ErrPoolInactive):
		return "storage is inactive on the configured node"
	case errors.Is(err, domain.ErrStorage):
		return "storage does not support CSI volumes"
	case errors.Is(err, domain.ErrCapacity):
		return "storage has less than 1 GiB available, insufficient for the minimum CSI volume"
	case errors.Is(err, ErrBusy):
		return "VM is locked or a related background operation is active; wait for it to finish and retry"
	default:
		return ""
	}
}
