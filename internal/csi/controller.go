package csidriver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"pve-csi.local/csi-plugin/internal/domain"
)

type Config struct {
	Node      string
	OwnerVMID int
}

type Controller struct {
	csi.UnimplementedControllerServer
	cfg       Config
	allocator Provisioner
	reader    VolumeReader
}

func NewController(cfg Config, allocator Provisioner, reader VolumeReader) (*Controller, error) {
	if allocator == nil {
		return nil, errors.New("CSI controller requires a volume allocator")
	}
	if reader == nil {
		return nil, errors.New("CSI controller requires a volume reader")
	}
	if err := domain.ValidateOwnerScope(cfg.Node, cfg.OwnerVMID); err != nil {
		return nil, fmt.Errorf("invalid CSI controller configuration: %w", err)
	}
	return &Controller{cfg: cfg, allocator: allocator, reader: reader}, nil
}

func (c *Controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, csiBackendError(err)
	}
	backendReq, err := c.volumeRequest(req)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "[validate_request] %s", status.Convert(err).Message())
	}
	volume, err := c.allocator.EnsureVolume(ctx, backendReq)
	if err != nil {
		return nil, csiBackendError(err)
	}
	if !volume.CompatibleWith(backendReq) {
		return nil, status.Error(codes.Internal, "allocator returned an inconsistent volume")
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: volume.ID(), CapacityBytes: volume.CapacityBytes(),
		AccessibleTopology: []*csi.Topology{{Segments: map[string]string{pveNodeTopologyKey: volume.Placement().Node}}},
	}}, nil
}

func (c *Controller) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	// Advertise CREATE_DELETE_VOLUME only after DeleteVolume is implemented.
	return &csi.ControllerGetCapabilitiesResponse{}, nil
}

// Contracts consumed by CSI; implemented by the volume application services.
type Provisioner interface {
	EnsureVolume(context.Context, domain.Request) (domain.Volume, error)
}
type VolumeReader interface {
	GetVolume(context.Context, domain.Identity) (domain.Volume, error)
}

const pveNodeTopologyKey = "pve-csi.local/pve-node"

func (c *Controller) volumeRequest(req *csi.CreateVolumeRequest) (domain.Request, error) {
	invalid := func(message string) (domain.Request, error) {
		return domain.Request{}, status.Error(codes.InvalidArgument, message)
	}
	if req == nil || req.Name == "" || len(req.Name) > 128 || !utf8.ValidString(req.Name) || strings.ContainsFunc(req.Name, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'
	}) {
		return invalid("name must be 1 to 128 bytes of valid UTF-8 without prohibited control characters")
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
	// Preserve every original byte (including allowed whitespace) while passing
	// a safe name to the stricter backend. Keep this namespace stable on retry.
	backendReq := domain.Request{
		Name: domain.ProvisioningName(req.Name), Node: c.cfg.Node, Storage: storage,
		OwnerVMID: c.cfg.OwnerVMID, CapacityBytes: capacity, CapacityLimitBytes: limit,
	}
	// A selected pool must produce a handle that subsequent reads can parse.
	// Reject an oversized handle before any backend operation, not after POST.
	if _, err := domain.ParseHandle(backendReq.Handle()); err != nil {
		return invalid("node, storage and owner produce an invalid volume ID or exceed its 128-byte limit")
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
			node := segments[pveNodeTopologyKey]
			if len(segments) != 1 || !domain.ValidNodeName(node) {
				return status.Error(codes.InvalidArgument, "topology must specify only a valid PVE node segment")
			}
		}
	}
	// Preferred is advisory. An empty requisite list imposes no restriction.
	if len(requirements.Requisite) != 0 {
		for _, topology := range requirements.Requisite {
			if topology.Segments[pveNodeTopologyKey] == c.cfg.Node {
				return nil
			}
		}
		return status.Error(codes.ResourceExhausted, "configured PVE node is outside the requisite topology")
	}
	return nil
}

// Shared policy for creation and validation. This does not inspect a guest
// filesystem or imply that Node/attachment support is already implemented.
func supportedMountCapability(capability *csi.VolumeCapability) bool {
	mount := capability.GetMount()
	return mount != nil && capability.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER &&
		(mount.FsType == "" || mount.FsType == "ext4") && len(mount.MountFlags) == 0 && mount.VolumeMountGroup == ""
}
