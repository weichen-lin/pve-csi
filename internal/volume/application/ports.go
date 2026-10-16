package application

import (
	"context"

	"pve-csi.local/csi-plugin/internal/volume/domain"
)

type PoolState struct{ AvailableBytes int64 }
type AllocationReceipt struct{ Reference string }

// StorageGateway is owned by this use case. Lookup false means not observed;
// permission filtering or a remote write can still hide an existing allocation.
type StorageGateway interface {
	InspectPool(context.Context, domain.Placement) (PoolState, error)
	Lookup(context.Context, domain.Request) (domain.Allocation, bool, error)
	Allocate(context.Context, domain.Request) (AllocationReceipt, error)
}

type Provisioner interface {
	EnsureVolume(context.Context, domain.Request) (domain.Volume, error)
}

// ReadGateway deliberately exposes no allocation or deletion operation.
type ReadGateway interface {
	InspectPool(context.Context, domain.Placement) (PoolState, error)
	LookupVolume(context.Context, domain.Identity) (domain.Allocation, bool, error)
}

type VolumeReader interface {
	GetVolume(context.Context, domain.Identity) (domain.Volume, error)
}
