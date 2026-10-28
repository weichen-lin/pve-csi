package csidriver

import (
	"context"
	"errors"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// CheckHealth probes CSI readiness, not PVE connectivity, to avoid restart storms.
// Callers must set a deadline.
func CheckHealth(ctx context.Context, endpoint string) error {
	if _, err := SocketPath(endpoint); err != nil {
		return err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return errors.New("CSI health check requires a deadline")
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	response, err := csi.NewIdentityClient(conn).Probe(ctx, &csi.ProbeRequest{})
	if err != nil {
		return fmt.Errorf("CSI Probe failed: %w", err)
	}
	// The CSI specification defines an omitted ready field as ready.
	if response == nil || (response.Ready != nil && !response.Ready.Value) {
		return errors.New("CSI is still initializing")
	}
	return nil
}
