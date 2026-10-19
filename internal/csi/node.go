package csidriver

import (
	"context"
	"errors"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"pve-csi.local/csi-plugin/internal/domain"
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

// Node currently advertises identity only. Mount RPCs remain Unimplemented.
type Node struct {
	csi.UnimplementedNodeServer
	cfg NodeConfig
	id  domain.NodeID
}

func NewNode(cfg NodeConfig, rawUUID string) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	id, err := domain.ParseNodeID(rawUUID)
	if err != nil {
		return nil, err
	}
	return &Node{cfg: cfg, id: id}, nil
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
	return &csi.NodeGetCapabilitiesResponse{}, nil
}
