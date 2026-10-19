package csidriver

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/domain"
)

// Log only allowlisted identifiers and our own error translations. Never dump
// protobuf requests, responses, metadata, raw backend errors or credentials.
func RequestLogger(logger *slog.Logger) grpc.UnaryServerInterceptor {
	var sequence atomic.Uint64
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == "/csi.v1.Identity/Probe" {
			return handler(ctx, req)
		}
		attrs := []any{"request_id", sequence.Add(1), "method", info.FullMethod}
		if r, ok := req.(interface{ GetVolumeId() string }); ok {
			if id, err := domain.ParseHandle(r.GetVolumeId()); err == nil {
				attrs = append(attrs, "volume_id", id.ID())
			}
		}
		if r, ok := req.(*csi.CreateVolumeRequest); ok && r.GetName() != "" {
			attrs = append(attrs, "volume_name_hash", domain.NameDigest(r.GetName()))
		}
		if r, ok := req.(interface{ GetNodeId() string }); ok {
			if id, err := domain.ParseNodeID(r.GetNodeId()); err == nil {
				attrs = append(attrs, "node_id", id.String())
			}
		}
		requestLog := logger.With(attrs...)
		started := time.Now()
		requestLog.InfoContext(ctx, "csi_request_started")
		response, err := handler(ctx, req)
		result := []any{"duration_ms", time.Since(started).Milliseconds(), "status", status.Code(err).String()}
		var failure *operationStatus
		if errors.As(err, &failure) {
			result = append(result, "step", failure.step, "detail", failure.status.Message())
		}
		if err != nil {
			requestLog.ErrorContext(ctx, "csi_request_finished", result...)
		} else {
			requestLog.InfoContext(ctx, "csi_request_finished", result...)
		}
		return response, err
	}
}
