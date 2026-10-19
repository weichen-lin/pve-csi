package volume

import (
	"context"
	"errors"

	"pve-csi.local/csi-plugin/internal/domain"
)

var ErrAttachmentUnconfirmed = errors.New("attachment change outcome is unknown; retry the same request")

type AttachmentGateway interface {
	NodeInventory
	References(context.Context, domain.Identity) (domain.References, error)
	Attach(context.Context, domain.Identity, domain.VirtualMachine) error
	Detach(context.Context, domain.Identity, domain.DiskReference) error
}

func (s *Service) Publish(ctx context.Context, id domain.Identity, nodeID string) (string, error) {
	if err := id.Validate(); err != nil {
		return "", err
	}
	if !s.scope.Contains(id.Placement(), id.OwnerVMID()) {
		return "", ErrOutsideScope
	}
	requested, err := domain.ParseNodeID(nodeID)
	if err != nil {
		return "", AtStep("validate_node_id", "node", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !s.acquire(id.ID()) {
		return "", ErrBusy
	}
	defer s.release(id.ID())
	// Serializes slot selection and reference scans across volumes in this controller.
	if !s.vmMu.TryLock() {
		return "", ErrBusy
	}
	defer s.vmMu.Unlock()
	if _, err := s.GetVolume(ctx, id); err != nil {
		return "", err
	}
	if err := s.CheckReservedOwner(ctx); err != nil {
		return "", err
	}
	vm, err := s.resolveNode(ctx, requested)
	if err != nil {
		return "", err
	}
	if err := id.CheckAttachmentTarget(vm); err != nil {
		return "", err
	}
	refs, err := s.attachments.References(ctx, id)
	if err != nil {
		return "", AtStep("inspect_attachments", id.Reference(), err)
	}
	attached, err := refs.CheckAttach(id, vm)
	if err != nil {
		return "", AtStep("check_single_node", id.Reference(), err)
	}
	if attached {
		return id.Serial(), nil
	}
	writeErr := s.attachments.Attach(ctx, id, vm)
	refs, readErr := s.attachments.References(ctx, id)
	attached, checkErr := refs.CheckAttach(id, vm)
	if readErr == nil && checkErr == nil && attached && !errors.Is(writeErr, ErrAttachmentUnconfirmed) {
		return id.Serial(), nil
	}
	if writeErr != nil && !errors.Is(writeErr, ErrAttachmentUnconfirmed) {
		return "", writeErr
	}
	return "", AtStep("confirm_attachment", id.Reference(), errors.Join(ErrAttachmentUnconfirmed, writeErr, readErr, checkErr))
}

func (s *Service) Unpublish(ctx context.Context, id domain.Identity, nodeID string) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if !s.scope.Contains(id.Placement(), id.OwnerVMID()) {
		return ErrOutsideScope
	}
	var requested domain.NodeID
	if nodeID != "" {
		var err error
		requested, err = domain.ParseNodeID(nodeID)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.acquire(id.ID()) {
		return ErrBusy
	}
	defer s.release(id.ID())
	if !s.vmMu.TryLock() {
		return ErrBusy
	}
	defer s.vmMu.Unlock()
	refs, err := s.attachments.References(ctx, id)
	if err != nil {
		return AtStep("inspect_attachments", id.Reference(), err)
	}
	target, found, err := refs.DetachTarget(id, requested)
	if err != nil || !found {
		return err
	}
	writeErr := s.attachments.Detach(ctx, id, target)
	refs, readErr := s.attachments.References(ctx, id)
	if readErr == nil && !errors.Is(writeErr, ErrAttachmentUnconfirmed) && refs.DetachedFrom(requested) {
		return nil
	}
	if writeErr != nil && !errors.Is(writeErr, ErrAttachmentUnconfirmed) {
		return writeErr
	}
	return AtStep("confirm_detachment", id.Reference(), errors.Join(ErrAttachmentUnconfirmed, writeErr, readErr))
}
