package main

import (
	"context"
	"fmt"
	"os"
	"pve-csi.local/csi-plugin/internal/bootstrap"
)

func main() {
	if err := bootstrap.RunQuery(context.Background(), os.Stdout, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "pve-csi: %v\n", err)
		os.Exit(1)
	}
}
