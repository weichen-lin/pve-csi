package csidriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestRequestLogsIdentifyFailureWithoutLeakingData(t *testing.T) {
	const secret = "DO-NOT-LOG-THIS"
	req, err := domain.NewRequest("one", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHandle(req.Handle())
	for _, raw := range []string{id.ID(), secret} {
		var output bytes.Buffer
		logger := RequestLogger(slog.New(slog.NewJSONHandler(&output, nil)))
		req := &csi.NodePublishVolumeRequest{VolumeId: raw, TargetPath: secret, Secrets: map[string]string{"token": secret}, PublishContext: map[string]string{"secret": secret}}
		_, err := logger(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Node/NodePublishVolume"}, func(context.Context, any) (any, error) {
			return nil, csiReadError(volume.AtStep("publish_volume", id.Reference(), volume.NewBackendError(volume.FailurePermissionDenied, "operation denied", errors.New(secret))))
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), secret) {
			t.Fatal("request or backend data leaked")
		}
		lines := strings.Split(strings.TrimSpace(output.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("missing start/end record: %s", output.String())
		}
		var start, end map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(lines[1]), &end); err != nil {
			t.Fatal(err)
		}
		if start["request_id"] != end["request_id"] || end["step"] != "publish_volume" || end["status"] != "PermissionDenied" || end["duration_ms"] == nil {
			t.Fatalf("incomplete diagnostics: %v", end)
		}
		if raw == id.ID() && end["volume_id"] != id.ID() {
			t.Fatal("missing managed disk identity")
		}
	}
}

func TestRequestLoggerNeverDumpsUnexpectedErrorsOrCreateName(t *testing.T) {
	const secret = "sensitive-name-and-error"
	var output bytes.Buffer
	logger := RequestLogger(slog.New(slog.NewJSONHandler(&output, nil)))
	req := &csi.CreateVolumeRequest{Name: secret, Parameters: map[string]string{"secret": secret}}
	_, _ = logger(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Controller/CreateVolume"}, func(context.Context, any) (any, error) { return nil, errors.New(secret) })
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), domain.NameDigest(secret)) {
		t.Fatalf("unsafe or missing correlation: %s", output.String())
	}
	output.Reset()
	_, _ = logger(context.Background(), &csi.ProbeRequest{}, &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Identity/Probe"}, func(context.Context, any) (any, error) { return &csi.ProbeResponse{}, nil })
	if output.Len() != 0 {
		t.Fatal("routine probes flooded logs")
	}
}
