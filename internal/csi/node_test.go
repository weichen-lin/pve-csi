package csidriver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestNodeRPCIdentitySurvivesRestart(t *testing.T) {
	const uuid = "153a7c9e-588b-483f-884a-4eba01bc160a"
	for range 2 {
		func() {
			node, err := NewNode(NodeConfig{DriverName: "example.csi", PVENode: "host-a"}, strings.ToUpper(uuid))
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			RegisterNode(server, node, "test")
			listener := bufconn.Listen(1 << 20)
			defer listener.Close()
			defer server.Stop()
			go server.Serve(listener)
			conn, err := grpc.NewClient("passthrough:///node", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := csi.NewNodeClient(conn)
			info, err := client.NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
			if err != nil || info.NodeId != uuid || len(info.AccessibleTopology.Segments) != 1 || info.AccessibleTopology.Segments["example.csi/pve-node"] != "host-a" {
				t.Fatalf("NodeGetInfo = %v, %v", info, err)
			}
			caps, err := client.NodeGetCapabilities(ctx, &csi.NodeGetCapabilitiesRequest{})
			if err != nil || len(caps.Capabilities) != 0 {
				t.Fatalf("Node capabilities = %v, %v", caps, err)
			}
			identity := csi.NewIdentityClient(conn)
			plugin, err := identity.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
			if err != nil || plugin.Name != "example.csi" {
				t.Fatalf("plugin identity = %v, %v", plugin, err)
			}
			pluginCaps, err := identity.GetPluginCapabilities(ctx, &csi.GetPluginCapabilitiesRequest{})
			if err != nil {
				t.Fatal(err)
			}
			for _, capability := range pluginCaps.Capabilities {
				if capability.GetService().GetType() == csi.PluginCapability_Service_CONTROLLER_SERVICE {
					t.Fatal("Node advertised Controller service")
				}
			}
			if _, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{}); status.Code(err) != codes.Unimplemented {
				t.Fatalf("mount must remain unimplemented: %v", err)
			}
			if _, err := csi.NewControllerClient(conn).ControllerGetCapabilities(ctx, &csi.ControllerGetCapabilitiesRequest{}); status.Code(err) != codes.Unimplemented {
				t.Fatalf("Controller registered on Node: %v", err)
			}
		}()
	}
}

func TestNodeErrorClassification(t *testing.T) {
	for _, tt := range []struct {
		err  error
		code codes.Code
	}{
		{domain.ErrInvalidNodeID, codes.InvalidArgument},
		{domain.ErrNodeNotFound, codes.NotFound},
		{domain.ErrNodeAmbiguous, codes.FailedPrecondition},
		{domain.ErrInvalidInventory, codes.Internal},
		{volume.NewBackendError(volume.FailurePermissionDenied, "", nil), codes.PermissionDenied},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{context.Canceled, codes.Canceled},
	} {
		err := csiBackendError(volume.AtStep("resolve_node", "host-a", tt.err))
		if status.Code(err) != tt.code || !strings.Contains(err.Error(), "[resolve_node] host-a") {
			t.Errorf("got %v, want %v", err, tt.code)
		}
	}
}
