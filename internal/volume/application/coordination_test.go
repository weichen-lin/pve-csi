package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pve-csi.local/csi-plugin/internal/volume/domain"
)

type provisionFunc func(context.Context, domain.Request) (domain.Volume, error)

func (f provisionFunc) EnsureVolume(ctx context.Context, req domain.Request) (domain.Volume, error) {
	return f(ctx, req)
}

func TestCoordinatorSharedCallersAndErrorCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	coordinator, err := NewCoordinator(provisionFunc(func(ctx context.Context, req domain.Request) (domain.Volume, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return domain.Volume{}, ctx.Err()
			}
			return domain.Volume{}, errors.New("first call failed")
		}
		return domain.Confirm(req, domain.Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes})
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Separate callers receive the same injected coordinator instance.
	var first, second Provisioner = coordinator, coordinator
	done := make(chan error, 1)
	go func() { _, err := first.EnsureVolume(ctx, testRequest()); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first caller did not enter")
	}
	if _, err := second.EnsureVolume(ctx, testRequest()); !errors.Is(err, ErrBusy) || calls.Load() != 1 {
		t.Fatalf("overlapping caller reached backend: %v", err)
	}
	other := testRequest()
	other.Name = "other"
	if _, err := second.EnsureVolume(ctx, other); err != nil {
		t.Fatalf("different name was blocked: %v", err)
	}
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected first-call failure")
		}
	case <-ctx.Done():
		t.Fatal("first caller did not finish")
	}
	if _, err := second.EnsureVolume(ctx, testRequest()); err != nil {
		t.Fatalf("error did not release guard: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("backend calls = %d, want 3", calls.Load())
	}
}
