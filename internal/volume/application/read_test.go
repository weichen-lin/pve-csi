package application

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"pve-csi.local/csi-plugin/internal/volume/domain"
)

// Intentionally has no Allocate method: reads cannot call the write port.
type readGateway struct {
	calls    []string
	observed domain.Allocation
	found    bool
	err      error
}

func (g *readGateway) InspectPool(ctx context.Context, _ domain.Placement) (PoolState, error) {
	g.calls = append(g.calls, "inspect")
	return PoolState{AvailableBytes: 0}, ctx.Err()
}

func (g *readGateway) LookupVolume(ctx context.Context, id domain.Identity) (domain.Allocation, bool, error) {
	g.calls = append(g.calls, "lookup:"+id.Reference())
	if err := ctx.Err(); err != nil {
		return domain.Allocation{}, false, err
	}
	return g.observed, g.found, g.err
}

func TestReadVolumeObservesWithoutAllocation(t *testing.T) {
	req := testRequest()
	id, err := domain.ParseHandle(req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	gateway := &readGateway{found: true, observed: domain.Allocation{Reference: req.AllocationReference(), Placement: req.Placement(), OwnerVMID: req.OwnerVMID, CapacityBytes: req.CapacityBytes}}
	reader, err := NewReader(gateway, req.Placement(), req.OwnerVMID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		volume, err := reader.GetVolume(context.Background(), id)
		if err != nil || !volume.CompatibleWith(req) {
			t.Fatalf("read = %+v, %v", volume, err)
		}
	}
	if !reflect.DeepEqual(gateway.calls, []string{"inspect", "lookup:" + id.Reference(), "inspect", "lookup:" + id.Reference()}) {
		t.Fatalf("read calls = %v", gateway.calls)
	}
	gateway.found = false
	if _, err := reader.GetVolume(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	gateway.err = errors.New("lookup failed")
	if _, err := reader.GetVolume(context.Background(), id); !errors.Is(err, gateway.err) {
		t.Fatalf("failed lookup became not found: %v", err)
	}
	gateway.err = nil
	gateway.found = true
	gateway.observed.OwnerVMID++
	if _, err := reader.GetVolume(context.Background(), id); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("wrong owner = %v", err)
	}
}

func TestReadVolumeChecksScopeAndCancellationBeforeIO(t *testing.T) {
	req := testRequest()
	gateway := &readGateway{}
	reader, err := NewReader(gateway, req.Placement(), req.OwnerVMID)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*domain.Request){
		func(r *domain.Request) { r.Node = "other" },
		func(r *domain.Request) { r.Storage = "other" },
		func(r *domain.Request) { r.OwnerVMID++ },
	} {
		foreign := req
		change(&foreign)
		id, err := domain.ParseHandle(foreign.Handle())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.GetVolume(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign identity = %v", err)
		}
	}
	if _, err := reader.GetVolume(context.Background(), domain.Identity{}); !errors.Is(err, domain.ErrInvalidHandle) {
		t.Fatalf("zero identity = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, err := domain.ParseHandle(req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetVolume(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	if len(gateway.calls) != 0 {
		t.Fatalf("invalid read reached gateway: %v", gateway.calls)
	}
	if _, err := NewReader(nil, req.Placement(), req.OwnerVMID); err == nil {
		t.Fatal("nil gateway accepted")
	}
	if _, err := NewReader(gateway, req.Placement(), 0); err == nil {
		t.Fatal("invalid owner accepted")
	}
}
