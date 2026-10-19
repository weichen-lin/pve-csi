package volume

import (
	"context"
	"errors"
	"testing"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
)

type nodeInventoryFunc func(context.Context, string) ([]domain.VirtualMachine, error)

func (f nodeInventoryFunc) VirtualMachines(ctx context.Context, node string) ([]domain.VirtualMachine, error) {
	return f(ctx, node)
}

func TestNodeResolverDoesNotCacheOrAcceptPartialResults(t *testing.T) {
	const uuid = "153a7c9e-588b-483f-884a-4eba01bc160a"
	id, _ := domain.ParseNodeID(uuid)
	calls := 0
	inventory := nodeInventoryFunc(func(_ context.Context, node string) ([]domain.VirtualMachine, error) {
		calls++
		machines := []domain.VirtualMachine{{Node: node, VMID: 100 + calls, ID: id}}
		if calls == 3 {
			return machines, context.DeadlineExceeded
		}
		return machines, nil
	})
	resolver, err := NewNodeResolver(inventory, "pve")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{101, 102} {
		vm, err := resolver.Resolve(context.Background(), uuid)
		if err != nil || vm.VMID != want {
			t.Fatalf("Resolve = %v, %v; want VM%d", vm, err, want)
		}
	}
	vm, err := resolver.Resolve(context.Background(), uuid)
	if !errors.Is(err, context.DeadlineExceeded) || vm != (domain.VirtualMachine{}) {
		t.Fatalf("accepted partial inventory: %v, %v", vm, err)
	}
	if _, err := resolver.Resolve(context.Background(), "bad"); !errors.Is(err, domain.ErrInvalidNodeID) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := resolver.Resolve(ctx, uuid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("invalid or expired requests reached backend: %d", calls)
	}
}
