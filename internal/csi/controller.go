package csidriver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

type Config struct {
	DriverName string
}

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// CSI limits both the driver name and the optional topology prefix to 63 bytes.
func validDNSName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !dnsLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func (cfg Config) Validate() error {
	if !validDNSName(cfg.DriverName) {
		return errors.New("CSI_DRIVER_NAME is required and must be a lowercase DNS name of at most 63 bytes")
	}
	return nil
}

func (cfg Config) TopologyKey() string { return cfg.DriverName + "/pve-node" }

type Controller struct {
	csi.UnimplementedControllerServer
	cfg     Config
	service *volume.Service
}

func NewController(cfg Config, service *volume.Service) (*Controller, error) {
	if service == nil {
		return nil, errors.New("CSI controller requires a volume service")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid CSI controller configuration: %w", err)
	}
	return &Controller{cfg: cfg, service: service}, nil
}

func (c *Controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, csiBackendError(err)
	}
	backendReq, err := c.volumeRequest(req)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "[validate_request] %s", status.Convert(err).Message())
	}
	volume, err := c.service.EnsureVolume(ctx, backendReq)
	if err != nil {
		return nil, csiBackendError(err)
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: volume.ID(), CapacityBytes: volume.CapacityBytes(),
		AccessibleTopology: []*csi.Topology{{Segments: map[string]string{c.cfg.TopologyKey(): volume.Placement().Node}}},
	}}, nil
}

func (c *Controller) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: []*csi.ControllerServiceCapability{{
		Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{
			Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		}},
	}, {Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME}}}}}, nil
}

func (c *Controller) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, csiDeleteError(err)
	}
	if req == nil || req.VolumeId == "" || len(req.Secrets) != 0 {
		return nil, status.Error(codes.InvalidArgument, "[validate_delete_request] volume_id is required; per-request secrets are not supported")
	}
	id, err := domain.ParseHandle(req.VolumeId)
	if err != nil {
		return nil, csiDeleteError(volume.AtStep("validate_delete_request", "volume", err))
	}
	if err := c.service.DeleteVolume(ctx, id); err != nil {
		return nil, csiDeleteError(err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (c *Controller) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req == nil || req.NodeId == "" || req.Readonly || len(req.Secrets) != 0 || !supportedMountCapability(req.VolumeCapability) {
		return nil, status.Error(codes.InvalidArgument, "volume_id, node_id and ext4 SINGLE_NODE_WRITER capability are required; read-only attachment and secrets are unsupported")
	}
	id, err := domain.ParseHandle(req.VolumeId)
	if err != nil {
		return nil, csiReadError(err)
	}
	serial, err := c.service.Publish(ctx, id, req.NodeId)
	if err != nil {
		return nil, csiReadError(err)
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: map[string]string{"serial": serial}}, nil
}
func (c *Controller) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req == nil || len(req.Secrets) != 0 {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required; secrets are unsupported")
	}
	id, err := domain.ParseHandle(req.VolumeId)
	if err != nil {
		return nil, csiReadError(err)
	}
	if err := c.service.Unpublish(ctx, id, req.NodeId); err != nil {
		return nil, csiReadError(err)
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (c *Controller) volumeRequest(req *csi.CreateVolumeRequest) (domain.Request, error) {
	invalid := func(message string) (domain.Request, error) {
		return domain.Request{}, status.Error(codes.InvalidArgument, message)
	}
	if req == nil {
		return invalid("create volume request is required")
	}
	if req.VolumeContentSource != nil || len(req.MutableParameters) != 0 || len(req.Secrets) != 0 {
		return invalid("content sources, mutable parameters and per-request secrets are not supported")
	}
	for key := range req.Parameters {
		if key != "storage" {
			return invalid("only the storage parameter is supported")
		}
	}
	storage := req.Parameters["storage"]
	if !domain.ValidStorageID(storage) {
		return invalid("parameter storage is required and must be a valid PVE storage ID")
	}
	if len(req.VolumeCapabilities) == 0 {
		return invalid("at least one volume capability is required")
	}
	for _, capability := range req.VolumeCapabilities {
		if !supportedMountCapability(capability) {
			return invalid("only ext4 mount volumes with SINGLE_NODE_WRITER and no mount flags or mount group are supported")
		}
	}
	capacity, limit, err := csiCapacity(req.CapacityRange)
	if err != nil {
		return domain.Request{}, err
	}
	if err := c.validateTopology(req.AccessibilityRequirements); err != nil {
		return domain.Request{}, err
	}
	backendReq, err := domain.NewRequest(req.Name,
		domain.Placement{Node: c.service.Scope().Node, Storage: storage},
		c.service.Scope().OwnerVMID, capacity, limit)
	if err != nil {
		return invalid(err.Error())
	}
	return backendReq, nil
}

func csiCapacity(requested *csi.CapacityRange) (int64, int64, error) {
	capacity, limit, err := domain.NormalizeCapacity(requested.GetRequiredBytes(), requested.GetLimitBytes(), requested != nil)
	if err == nil {
		return capacity, limit, nil
	}
	if errors.Is(err, domain.ErrInvalidCapacity) {
		return 0, 0, status.Error(codes.InvalidArgument, err.Error())
	}
	return 0, 0, status.Error(codes.OutOfRange, err.Error())
}

func (c *Controller) validateTopology(requirements *csi.TopologyRequirement) error {
	if requirements == nil {
		return nil
	}
	if len(requirements.Requisite) == 0 && len(requirements.Preferred) == 0 {
		return status.Error(codes.InvalidArgument, "topology requirements must contain requisite or preferred entries")
	}
	for _, group := range [][]*csi.Topology{requirements.Requisite, requirements.Preferred} {
		for _, topology := range group {
			segments := topology.GetSegments()
			node := segments[c.cfg.TopologyKey()]
			if len(segments) != 1 || !domain.ValidNodeName(node) {
				return status.Errorf(codes.InvalidArgument, "topology must specify only %s with a valid PVE node name", c.cfg.TopologyKey())
			}
		}
	}
	// Preferred is advisory. An empty requisite list imposes no restriction.
	if len(requirements.Requisite) != 0 {
		for _, topology := range requirements.Requisite {
			if topology.Segments[c.cfg.TopologyKey()] == c.service.Scope().Node {
				return nil
			}
		}
		return status.Error(codes.ResourceExhausted, "configured PVE node is outside the requisite topology")
	}
	return nil
}

// Shared protocol policy; the Node application separately checks the actual filesystem.
func supportedMountCapability(capability *csi.VolumeCapability) bool {
	mount := capability.GetMount()
	return mount != nil && capability.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER &&
		(mount.FsType == "" || mount.FsType == "ext4") && len(mount.MountFlags) == 0 && mount.VolumeMountGroup == ""
}
