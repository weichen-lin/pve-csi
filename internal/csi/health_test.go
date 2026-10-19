package csidriver

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type probeIdentity struct {
	csi.UnimplementedIdentityServer
	state string
}

func (s probeIdentity) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	switch s.state {
	case "ready":
		return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
	case "omitted":
		return &csi.ProbeResponse{}, nil
	case "initializing":
		return &csi.ProbeResponse{Ready: wrapperspb.Bool(false)}, nil
	case "hung":
		<-ctx.Done()
		return nil, ctx.Err()
	default:
		return nil, status.Error(codes.FailedPrecondition, "unhealthy")
	}
}

func TestHealthChecksRPCInsteadOfSocketExistence(t *testing.T) {
	for _, state := range []string{"ready", "omitted", "initializing", "hung", "unhealthy", "absent"} {
		t.Run(state, func(t *testing.T) {
			path := socketTestPath(t)
			if state != "absent" {
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				server := grpc.NewServer()
				csi.RegisterIdentityServer(server, probeIdentity{state: state})
				defer server.Stop()
				go server.Serve(listener)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			err := CheckHealth(ctx, "unix://"+path)
			wantSuccess := state == "ready" || state == "omitted"
			if (err == nil) != wantSuccess {
				t.Fatalf("health=%v", err)
			}
			if state == "hung" && status.Code(err) != codes.DeadlineExceeded {
				t.Fatalf("unbounded or incorrect timeout: %v", err)
			}
		})
	}
}

func TestHealthRejectsUnboundedAndNetworkEndpoints(t *testing.T) {
	if err := CheckHealth(context.Background(), "unix:///tmp/unused.sock"); err == nil {
		t.Fatal("unbounded probe allowed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := CheckHealth(ctx, "tcp://example.com:9000"); err == nil {
		t.Fatal("network probe allowed")
	}
}
