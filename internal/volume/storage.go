package volume

import (
	"context"

	"pve-csi.local/csi-plugin/internal/domain"
)

// StorageGateway is the application's port to storage. InspectPool verifies
// read access; observations never authorize writes. The application decides
// when allocation/deletion checks are required, including idempotent retries.
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
