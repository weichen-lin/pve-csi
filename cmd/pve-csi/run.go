package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	csidriver "pve-csi.local/csi-plugin/internal/csi"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/linux"
	"pve-csi.local/csi-plugin/internal/pve"
	"pve-csi.local/csi-plugin/internal/volume"
)

func Run(ctx context.Context, out, usage io.Writer, getenv func(string) string, args []string) error {
	if len(args) == 0 {
		return errors.New("command required: controller, node, probe or check; use --help")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "Usage: pve-csi <controller|node|probe> [--endpoint unix:///path/to/csi.sock]\n       pve-csi check\nCheck settings come from .env or environment variables. Use the command with --help for details.")
		return err
	}
	if args[0] == "check" {
		return runCheck(ctx, out, usage, getenv, args[1:])
	}
	if args[0] != "controller" && args[0] != "node" && args[0] != "probe" {
		return fmt.Errorf("unknown command %q; use --help", args[0])
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(usage)
	endpoint := flags.String("endpoint", "unix:///tmp/pve-csi.sock", "local CSI Unix socket URL")
	var timeout *time.Duration
	if args[0] == "probe" {
		timeout = flags.Duration("timeout", 2*time.Second, "maximum CSI health check duration")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s accepts flags only", args[0])
	}
	if args[0] == "probe" {
		if *timeout <= 0 {
			return errors.New("probe timeout must be positive")
		}
		probeCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return csidriver.CheckHealth(probeCtx, *endpoint)
	}
	if version == "" {
		return errors.New("build version is missing; build with -ldflags '-X main.version=<git-tag-or-commit>'")
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(out, nil)))
	if args[0] == "node" {
		return runNode(ctx, out, getenv, *endpoint, os.ReadFile)
	}
	return runController(ctx, out, getenv, *endpoint)
}

func runNode(ctx context.Context, out io.Writer, getenv func(string) string, endpoint string, readFile func(string) ([]byte, error)) error {
	if _, err := csidriver.SocketPath(endpoint); err != nil {
		return err
	}
	cfg, err := LoadNodeConfig(getenv)
	if err != nil {
		return err
	}
	const uuidPath = "/sys/class/dmi/id/product_uuid"
	raw, err := readFile(uuidPath)
	if err != nil {
		return fmt.Errorf("[read_node_identity] cannot read %s; mount the host UUID file read-only and check its permissions: %w", uuidPath, err)
	}
	gateway := linux.NewMountGateway(cfg.DriverName)
	mountService, err := volume.NewNodeService(gateway, cfg.PVENode)
	if err != nil {
		return err
	}
	node, err := csidriver.NewNode(cfg, strings.TrimSpace(string(raw)), mountService)
	if err != nil {
		return fmt.Errorf("[read_node_identity] invalid VM UUID: %w", err)
	}
	if err := gateway.CheckStartup(ctx); err != nil {
		return startupFailure(err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(csidriver.RequestLogger(slog.New(slog.NewJSONHandler(out, nil)))))
	csidriver.RegisterNode(server, node, version)
	return csidriver.Serve(ctx, endpoint, server, out)
}

func runController(ctx context.Context, out io.Writer, getenv func(string) string, endpoint string) error {
	if _, err := csidriver.SocketPath(endpoint); err != nil {
		return err
	}
	cfg, err := LoadConfig(getenv)
	if err != nil {
		return err
	}
	client, err := pve.NewClient(cfg.Connection)
	if err != nil {
		return err
	}
	defer client.Close()
	return serveController(ctx, out, endpoint, cfg, client)
}

func serveController(ctx context.Context, out io.Writer, endpoint string, cfg Config, client *pve.Client) error {
	server := grpc.NewServer(grpc.UnaryInterceptor(csidriver.RequestLogger(slog.New(slog.NewJSONHandler(out, nil)))))
	gateway, err := pve.NewStorageGateway(client)
	if err != nil {
		return err
	}
	attachments, err := pve.NewAttachmentGateway(client)
	if err != nil {
		return err
	}
	service, err := volume.NewService(gateway, cfg.Scope, attachments)
	if err != nil {
		return err
	}
	controller, err := csidriver.NewController(cfg.Controller, service)
	if err != nil {
		return err
	}
	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = client.CheckAccess(startupCtx, cfg.Scope.Node)
	if err == nil {
		err = service.CheckReservedOwner(startupCtx)
	}
	cancel()
	if err != nil {
		return startupFailure(err)
	}
	csidriver.Register(server, controller, version)
	return csidriver.Serve(ctx, endpoint, server, out)
}

// Process-boundary text uses only authored hints, never a transport error or
// response body. Request-time errors keep their normal CSI classification.
func startupFailure(err error) error {
	detail := "startup check failed"
	var backend *volume.BackendError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		detail = "startup check timed out; check connectivity and retry"
	case errors.Is(err, context.Canceled):
		detail = "startup check canceled"
	case errors.Is(err, domain.ErrOwnerInUse):
		detail = domain.ErrOwnerInUse.Error()
	case errors.As(err, &backend) && backend.Hint() != "":
		detail = backend.Hint()
	}
	var step *volume.StepError
	if errors.As(err, &step) {
		return fmt.Errorf("startup failed: [%s] %s: %s", step.Step(), step.Target(), detail)
	}
	return fmt.Errorf("startup failed: %s", detail)
}

// Injected at build time with -ldflags "-X main.version=<git-tag-or-commit>".
var version string
