package csidriver

import (
	"context"
	"errors"
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/volume/application"
	"pve-csi.local/csi-plugin/internal/volume/domain"
)

type Config struct {
	Node      string
	Storage   string
	OwnerVMID int
}

type Controller struct {
	csi.UnimplementedControllerServer
	cfg       Config
	allocator application.Provisioner
	reader    application.VolumeReader
}

// NewController accepts a composed provisioner. Bootstrap supplies the shared
// application Coordinator, whose lifetime must span all requests of an instance.
func NewController(cfg Config, allocator application.Provisioner, reader application.VolumeReader) (*Controller, error) {
	if allocator == nil {
		return nil, errors.New("CSI controller requires a volume allocator")
	}
	if reader == nil {
		return nil, errors.New("CSI controller requires a volume reader")
	}
	probe := domain.Request{Name: "configuration-check", Node: cfg.Node, Storage: cfg.Storage, OwnerVMID: cfg.OwnerVMID, CapacityBytes: domain.GiB}
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("invalid CSI controller configuration: %w", err)
	}
	if len(probe.Handle()) > 128 {
		return nil, errors.New("CSI controller node, storage and owner produce a volume handle longer than 128 bytes")
	}
	return &Controller{cfg: cfg, allocator: allocator, reader: reader}, nil
}

func (c *Controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, csiBackendError(err)
	}
	backendReq, err := c.volumeRequest(req)
	if err != nil {
		return nil, err
	}
	volume, err := c.allocator.EnsureVolume(ctx, backendReq)
	if err != nil {
		return nil, csiBackendError(err)
	}
	if !volume.CompatibleWith(backendReq) {
		return nil, status.Error(codes.Internal, "allocator returned an inconsistent volume")
	}
	return createResponse(volume), nil
}

func (c *Controller) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	// CREATE_DELETE_VOLUME promises both RPCs. DeleteVolume is not implemented,
	// so this intermediate learning step is not ready for external-provisioner.
	return &csi.ControllerGetCapabilitiesResponse{}, nil
}
