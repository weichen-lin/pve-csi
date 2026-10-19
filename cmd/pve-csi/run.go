package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/grpc"

	csidriver "pve-csi.local/csi-plugin/internal/csi"
	"pve-csi.local/csi-plugin/internal/pve"
	"pve-csi.local/csi-plugin/internal/volume"
)

func Run(ctx context.Context, out, usage io.Writer, getenv func(string) string, args []string) error {
	if len(args) == 0 {
		return errors.New("command required: controller or node; use --help")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "Usage: pve-csi <controller|node> [--endpoint unix:///path/to/csi.sock]\nUse controller --help or node --help for flags.")
		return err
	}
	if args[0] != "controller" && args[0] != "node" {
		return fmt.Errorf("unknown command %q; use --help", args[0])
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(usage)
	endpoint := flags.String("endpoint", "unix:///tmp/pve-csi.sock", "local CSI Unix socket URL")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s accepts flags only", args[0])
	}
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
	node, err := csidriver.NewNode(cfg, strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("[read_node_identity] invalid VM UUID: %w", err)
	}
	server := grpc.NewServer()
	csidriver.RegisterNode(server, node, Version)
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
	server := grpc.NewServer()
	gateway, err := pve.NewStorageGateway(client)
	if err != nil {
		return err
	}
	service, err := volume.NewService(gateway, cfg.Scope)
	if err != nil {
		return err
	}
	controller, err := csidriver.NewController(cfg.Controller, service)
	if err != nil {
		return err
	}
	csidriver.Register(server, controller, Version)
	return csidriver.Serve(ctx, endpoint, server, out)
}

const Version = "dev-011"
