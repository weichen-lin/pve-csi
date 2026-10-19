package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestNodeNeedsOnlyNodeSettings(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "CSI_DRIVER_NAME":
			return "example.csi"
		case "PVE_NODE":
			return "host-a"
		default:
			t.Fatalf("Node read Controller setting %s", key)
			return ""
		}
	}
	if _, err := LoadNodeConfig(getenv); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, body string
		err        error
		want       string
	}{
		{"missing file", "", os.ErrNotExist, "read_node_identity"},
		{"unreadable file", "", os.ErrPermission, "permissions"},
		{"invalid UUID", "bad\n", nil, "invalid VM UUID"},
		{"empty UUID", "\n", nil, "invalid VM UUID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := runNode(context.Background(), io.Discard, getenv, "unix:///tmp/unused-node-test.sock", func(path string) ([]byte, error) {
				if path != "/sys/class/dmi/id/product_uuid" {
					t.Fatalf("unexpected identity source %s", path)
				}
				return []byte(tt.body), tt.err
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("lost cause: %v", err)
			}
		})
	}
	for _, missing := range []string{"CSI_DRIVER_NAME", "PVE_NODE"} {
		if _, err := LoadNodeConfig(func(key string) string {
			if key == missing {
				return ""
			}
			return getenv(key)
		}); err == nil {
			t.Errorf("accepted missing %s", missing)
		}
	}
}
