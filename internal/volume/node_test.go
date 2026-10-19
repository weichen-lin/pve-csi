package volume

import (
	"context"
	"errors"
	"testing"

	"pve-csi.local/csi-plugin/internal/domain"
)

func TestNodeLookupDoesNotCacheOrAcceptPartialResults(t *testing.T) {
	s, _, _, _ := volumeFixture(t)
	a := s.attachments.(*attachmentStub)
	id, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	for _, want := range []int{101, 102} {
		a.machines = []domain.VirtualMachine{{Node: "pve", VMID: want, ID: id}}
		vm, err := s.resolveNode(context.Background(), id)
		if err != nil || vm.VMID != want {
			t.Fatalf("lookup = %v, %v; want VM%d", vm, err, want)
		}
	}
	a.readErr = context.DeadlineExceeded
	vm, err := s.resolveNode(context.Background(), id)
	if !errors.Is(err, context.DeadlineExceeded) || vm != (domain.VirtualMachine{}) {
		t.Fatalf("accepted partial inventory: %v, %v", vm, err)
	}
	a.readErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.resolveNode(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if a.inventoryReads != 3 {
		t.Fatalf("expired request reached backend: %d reads", a.inventoryReads)
	}
	a.machines = nil
	_, err = s.resolveNode(context.Background(), id)
	var step *StepError
	if !errors.Is(err, domain.ErrNodeNotFound) || !errors.As(err, &step) || step.Step() != "resolve_node" {
		t.Fatalf("lookup lost failure classification: %v", err)
	}
}
