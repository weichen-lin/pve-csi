package csidriver

import (
	"context"
	"maps"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/volume/domain"
)

func (c *Controller) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, csiReadError(err)
	}
	if req == nil || req.VolumeId == "" || len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID and volume capabilities are required")
	}
	id, err := domain.ParseHandle(req.VolumeId)
	if err != nil {
		return nil, csiReadError(err)
	}
	if len(req.Secrets) != 0 || len(req.VolumeContext) != 0 {
		// CreateVolume emits no volume_context, and authentication is configured
		// on the driver. Do not silently confirm caller-supplied context/secrets.
		return nil, status.Error(codes.InvalidArgument, "per-request secrets and nonempty volume context are not supported")
	}
	for _, capability := range req.VolumeCapabilities {
		if capability == nil || (capability.GetMount() == nil && capability.GetBlock() == nil) ||
			capability.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_UNKNOWN {
			return nil, status.Error(codes.InvalidArgument, "each capability requires an access type and access mode")
		}
	}
	volume, err := c.reader.GetVolume(ctx, id)
	if err != nil {
		return nil, csiReadError(err)
	}
	if volume.ID() != id.ID() || volume.Reference() != id.Reference() || volume.Placement() != id.Placement() || volume.OwnerVMID() != id.OwnerVMID() || volume.CapacityBytes() < domain.GiB {
		return nil, status.Error(codes.Internal, "reader returned an inconsistent volume")
	}
	unsupported := func() (*csi.ValidateVolumeCapabilitiesResponse, error) {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: "requested capabilities or parameters are not supported"}, nil
	}
	// Existence is checked before returning an unsupported capability. Never
	// echo unknown protobuf fields as if this version had validated them.
	if len(req.MutableParameters) != 0 || len(req.ProtoReflect().GetUnknown()) != 0 {
		return unsupported()
	}
	for key, value := range req.Parameters {
		if key != "storage" || value != c.cfg.Storage {
			return unsupported()
		}
	}
	confirmed := make([]*csi.VolumeCapability, 0, len(req.VolumeCapabilities))
	for _, capability := range req.VolumeCapabilities {
		if !supportedMountCapability(capability) || len(capability.ProtoReflect().GetUnknown()) != 0 {
			return unsupported()
		}
		mount := capability.GetMount()
		mode := capability.GetAccessMode()
		if len(mount.ProtoReflect().GetUnknown()) != 0 || len(mode.ProtoReflect().GetUnknown()) != 0 {
			return unsupported()
		}
		confirmed = append(confirmed, &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: mount.FsType}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode.Mode},
		})
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeCapabilities: confirmed,
		Parameters:         maps.Clone(req.Parameters),
	}}, nil
}
