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
	pool, err := s.storage.InspectPool(ctx, id.Placement(), id.OwnerVMID())
	if err != nil {
		return err
	}
	if err := pool.CheckReadable(); err != nil {
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
	if err := s.storage.CheckDeletion(ctx, id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	deleteErr := s.storage.Delete(ctx, id)
	_, found, readErr := s.storage.LookupVolume(ctx, id)
	var backendErr *BackendError
	definiteRejection := !errors.Is(deleteErr, ErrDeleteUnconfirmed) && errors.As(deleteErr, &backendErr) &&
		(backendErr.Kind == FailureUnauthenticated || backendErr.Kind == FailurePermissionDenied || backendErr.Kind == FailureNotFound)
	finished := deleteErr == nil || definiteRejection || errors.Is(deleteErr, ErrDeleteFailed)
	if readErr == nil && !found && finished {
		return nil
	}
	if definiteRejection || errors.Is(deleteErr, ErrDeleteFailed) {
		return deleteErr
	}
	return AtStep("confirm_delete", id.Reference(), errors.Join(ErrDeleteUnconfirmed, deleteErr, readErr))
}
