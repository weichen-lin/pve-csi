package volume

import (
	"context"

	"pve-csi.local/csi-plugin/internal/domain"
)

// StorageGateway separates read observations from write authorization.
// The service checks write permissions on every operation, including retries.
type StorageGateway interface {
	InspectPool(context.Context, domain.Placement, int) (domain.Pool, error)
	LookupVolume(context.Context, domain.Identity) (domain.Allocation, bool, error)
	CheckIdle(context.Context, domain.Placement, int) error
	CheckAllocationPermission(context.Context, domain.Placement) error
	Allocate(context.Context, domain.Request) (string, error)
	CheckDeletionPermission(context.Context, domain.Placement) error
	OwnerUnused(context.Context, int) (bool, error)
	Delete(context.Context, domain.Identity) error
}
