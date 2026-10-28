package csidriver

import (
	"context"
	"errors"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

type NodeConfig struct {
	DriverName string
	PVENode    string
}

func (cfg NodeConfig) Validate() error {
	if err := (Config{DriverName: cfg.DriverName}).Validate(); err != nil {
		return err
	}
	if !domain.ValidNodeName(cfg.PVENode) {
		return errors.New("PVE_NODE is required and must be a valid PVE node name")
	}
	return nil
}

type Node struct {
	csi.UnimplementedNodeServer
	cfg     NodeConfig
	id      domain.NodeID
	service *volume.NodeService
}

func NewNode(cfg NodeConfig, rawUUID string, service *volume.NodeService) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	id, err := domain.ParseNodeID(rawUUID)
	if err != nil {
		return nil, err
	}
	if service == nil {
		return nil, errors.New("node requires a mount service")
	}
	return &Node{cfg: cfg, id: id, service: service}, nil
}

func (n *Node) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, csiBackendError(err)
	}
	return &csi.NodeGetInfoResponse{
		NodeId: n.id.String(),
		AccessibleTopology: &csi.Topology{Segments: map[string]string{
			(Config{DriverName: n.cfg.DriverName}).TopologyKey(): n.cfg.PVENode,
		}},
	}, nil
}

func (n *Node) NodeGetCapabilities(ctx context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, csiBackendError(err)
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: []*csi.NodeServiceCapability{{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME}}}}}, nil
}

func mountIdentity(raw string, cap *csi.VolumeCapability, publish map[string]string) (domain.Identity, error) {
	id, err := domain.ParseHandle(raw)
	if err != nil {
		return domain.Identity{}, csiReadError(err)
	}
	if !supportedMountCapability(cap) {
		return domain.Identity{}, status.Error(codes.InvalidArgument, "only ext4 SINGLE_NODE_WRITER without mount flags or mount group is supported")
	}
	if len(publish) != 1 || publish["serial"] != id.Serial() {
		return domain.Identity{}, status.Error(codes.InvalidArgument, "publish_context must contain the expected disk serial")
	}
	return id, nil
}
func (n *Node) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req == nil || req.StagingTargetPath == "" || len(req.Secrets) != 0 {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path is required; secrets are unsupported")
	}
	id, err := mountIdentity(req.VolumeId, req.VolumeCapability, req.PublishContext)
	if err != nil {
		return nil, err
	}
	if err := n.service.Stage(ctx, id, req.StagingTargetPath); err != nil {
		return nil, csiReadError(err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}
func (n *Node) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req == nil || req.TargetPath == "" || req.StagingTargetPath == "" || len(req.Secrets) != 0 {
		return nil, status.Error(codes.InvalidArgument, "target_path and staging_target_path are required; secrets are unsupported")
	}
	id, err := mountIdentity(req.VolumeId, req.VolumeCapability, req.PublishContext)
	if err != nil {
		return nil, err
	}
	if err := n.service.Publish(ctx, id, req.StagingTargetPath, req.TargetPath, req.Readonly); err != nil {
		return nil, csiReadError(err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}
func (n *Node) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req == nil || req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path is required")
	}
	id, err := domain.ParseHandle(req.VolumeId)
	if err != nil {
		return nil, csiReadError(err)
	}
	if err := n.service.Unpublish(ctx, id, req.TargetPath); err != nil {
		return nil, csiReadError(err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}
func (n *Node) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req == nil || req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path is required")
	}
	id, err := domain.ParseHandle(req.VolumeId)
	if err != nil {
		return nil, csiReadError(err)
	}
	if err := n.service.Unstage(ctx, id, req.StagingTargetPath); err != nil {
		return nil, csiReadError(err)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}
