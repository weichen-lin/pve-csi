package application

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"pve-csi.local/csi-plugin/internal/volume/domain"
)

type memoryGateway struct {
	calls        []string
	allocation   domain.Allocation
	found        bool
	available    int64
	writes       int
	postError    error
	wrongReceipt bool
	leaveMissing bool
	cancel       context.CancelFunc
}

func (g *memoryGateway) InspectPool(ctx context.Context, _ domain.Placement) (PoolState, error) {
	g.calls = append(g.calls, "inspect")
	return PoolState{AvailableBytes: g.available}, ctx.Err()
}

func (g *memoryGateway) Lookup(ctx context.Context, _ domain.Request) (domain.Allocation, bool, error) {
	g.calls = append(g.calls, "lookup")
	return g.allocation, g.found, ctx.Err()
}

func (g *memoryGateway) Allocate(ctx context.Context, req domain.Request) (AllocationReceipt, error) {
	g.calls = append(g.calls, "allocate")
	g.writes++
	if !g.leaveMissing {
		g.allocation = domain.Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes}
		g.found = true
	}
	if g.cancel != nil {
		g.cancel()
	}
	if g.wrongReceipt {
		return AllocationReceipt{Reference: "wrong-volume"}, nil
	}
	return AllocationReceipt{Reference: req.AllocationReference()}, g.postError
}

func testRequest() domain.Request {
	return domain.Request{Name: "abc", Node: "test-pve", Storage: "test-pool", OwnerVMID: 482, CapacityBytes: domain.GiB}
}

func TestProvisioningThroughPort(t *testing.T) {
	for _, tc := range []struct {
		name            string
		configure       func(*memoryGateway)
		wantUnconfirmed bool
	}{
		{"confirmed", func(*memoryGateway) {}, false},
		{"write error recovered by observation", func(g *memoryGateway) { g.postError = errors.New("reply lost") }, false},
		{"explicit wrong success ID", func(g *memoryGateway) { g.wrongReceipt = true }, true},
		{"not observed after write", func(g *memoryGateway) { g.leaveMissing = true }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &memoryGateway{available: 8 * domain.GiB}
			tc.configure(gateway)
			service, err := NewService(gateway)
			if err != nil {
				t.Fatal(err)
			}
			req := testRequest()
			volume, err := service.EnsureVolume(context.Background(), req)
			if tc.wantUnconfirmed {
				if !errors.Is(err, ErrUnconfirmed) {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || !volume.CompatibleWith(req) {
				t.Fatalf("result = %v, %v", volume, err)
			}
			if !reflect.DeepEqual(gateway.calls, []string{"inspect", "lookup", "allocate", "lookup"}) || gateway.writes != 1 {
				t.Fatalf("unexpected remote sequence: %v", gateway.calls)
			}
			if !tc.wantUnconfirmed {
				gateway.available = 0
				if _, err := service.EnsureVolume(context.Background(), req); err != nil || gateway.writes != 1 {
					t.Fatalf("retry did not reuse allocation: %v", err)
				}
			}
		})
	}
}

func TestProvisioningCancellationAfterWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gateway := &memoryGateway{available: 8 * domain.GiB, cancel: cancel}
	service, err := NewService(gateway)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.EnsureVolume(ctx, testRequest())
	if !errors.Is(err, ErrUnconfirmed) || !errors.Is(err, context.Canceled) || gateway.writes != 1 {
		t.Fatalf("cancellation escaped caller context or replayed write: %v", err)
	}
}

func TestInvalidProvisioningInputDoesNotUseGateway(t *testing.T) {
	gateway := &memoryGateway{available: 8 * domain.GiB}
	service, err := NewService(gateway)
	if err != nil {
		t.Fatal(err)
	}
	req := testRequest()
	req.OwnerVMID = 0
	if _, err := service.EnsureVolume(context.Background(), req); err == nil || len(gateway.calls) != 0 {
		t.Fatal("invalid input reached storage gateway")
	}
	if _, err := NewService(nil); err == nil {
		t.Fatal("nil storage gateway accepted")
	}
}
