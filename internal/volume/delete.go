package volume

import (
	"context"
	"errors"

	"pve-csi.local/csi-plugin/internal/domain"
)

func (s *Service) DeleteVolume(ctx context.Context, id domain.Identity) error {
	if err := id.Validate(); err != nil {
		return AtStep("validate_delete_request", "volume", err)
	}
	if !s.scope.Contains(id.Placement(), id.OwnerVMID()) {
		return AtStep("validate_delete_scope", "volume", ErrOutsideScope)
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
	pool, err := s.storage.InspectPool(ctx, id.Placement(), id.OwnerVMID())
	if err != nil {
		return err
	}
	if err := pool.CheckReady(); err != nil {
		return AtStep("check_storage_status", id.Reference(), err)
	}
	if err := s.storage.CheckIdle(ctx, id.Placement(), id.OwnerVMID()); err != nil {
		return err
	}
	observed, found, err := s.storage.LookupVolume(ctx, id)
	if err != nil {
		return AtStep("lookup_delete_volume", id.Reference(), err)
	}
	if !found {
		return nil
	}
	if _, err := domain.Restore(id, observed); err != nil {
		return AtStep("validate_delete_volume", id.Reference(), err)
	}
	if err := s.storage.CheckDeletionPermission(ctx, id.Placement()); err != nil {
		return AtStep("check_deletion_permission", id.Reference(), err)
	}
	if err := s.CheckReservedOwner(ctx); err != nil {
		return err
	}
	refs, err := s.attachments.References(ctx, id)
	if err != nil {
		return AtStep("check_deletion_references", id.Reference(), err)
	}
	if err := refs.CheckDelete(); err != nil {
		return AtStep("check_deletion_references", id.Reference(), err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	deleteErr := s.storage.Delete(ctx, id)
	_, found, readErr := s.storage.LookupVolume(ctx, id)
	definiteRejection := rejectedWrite(deleteErr)
	finished := deleteErr == nil || definiteRejection || errors.Is(deleteErr, ErrDeleteFailed)
	if readErr == nil && !found && finished {
		return nil
	}
	if definiteRejection || errors.Is(deleteErr, ErrDeleteFailed) {
		return deleteErr
	}
	return AtStep("confirm_delete", id.Reference(), errors.Join(ErrDeleteUnconfirmed, deleteErr, readErr))
}
