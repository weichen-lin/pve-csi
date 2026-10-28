package volume

import (
	"context"
	"errors"
	"sync"

	"pve-csi.local/csi-plugin/internal/domain"
)

var (
	ErrInvalidMountPath = errors.New("mount path is outside the expected kubelet volume directory")
	ErrDeviceNotFound   = errors.New("expected disk serial is not visible yet; retry")
	ErrUnsafeMount      = errors.New("device or filesystem identity is unsafe; operation refused")
	ErrMountInUse       = errors.New("volume still has published mounts")
	ErrMountConflict    = errors.New("target is already published with a different readonly setting; unpublish it before changing the setting")
)

type MountGateway interface {
	Stage(context.Context, domain.Identity, string) error
	Publish(context.Context, domain.Identity, string, string, bool) error
	Unpublish(context.Context, domain.Identity, string) error
	Unstage(context.Context, domain.Identity, string) error
}

// NodeService serializes mounts using filesystem identity and kernel mount state.
type NodeService struct {
	gateway MountGateway
	node    string
	mu      sync.Mutex
}

func NewNodeService(gateway MountGateway, node string) (*NodeService, error) {
	if gateway == nil || !domain.ValidNodeName(node) {
		return nil, errors.New("node service requires a mount gateway and PVE node")
	}
	return &NodeService{gateway: gateway, node: node}, nil
}

func (s *NodeService) perform(ctx context.Context, id domain.Identity, step string, fn func() error) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if id.Placement().Node != s.node {
		return ErrOutsideScope
	}
	if !s.mu.TryLock() {
		return ErrBusy
	}
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return AtStep(step, id.Reference(), fn())
}
func (s *NodeService) Stage(ctx context.Context, id domain.Identity, path string) error {
	return s.perform(ctx, id, "stage_volume", func() error { return s.gateway.Stage(ctx, id, path) })
}
func (s *NodeService) Publish(ctx context.Context, id domain.Identity, stage, target string, readOnly bool) error {
	return s.perform(ctx, id, "publish_volume", func() error { return s.gateway.Publish(ctx, id, stage, target, readOnly) })
}
func (s *NodeService) Unpublish(ctx context.Context, id domain.Identity, path string) error {
	return s.perform(ctx, id, "unpublish_volume", func() error { return s.gateway.Unpublish(ctx, id, path) })
}
func (s *NodeService) Unstage(ctx context.Context, id domain.Identity, path string) error {
	return s.perform(ctx, id, "unstage_volume", func() error { return s.gateway.Unstage(ctx, id, path) })
}
