package bootstrap

import (
	"context"
	"fmt"
	"io"
	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"runtime"
	"sort"
	"strings"
)

const Version = "dev-005"

func RunQuery(ctx context.Context, out io.Writer, getenv func(string) string) error {
	fmt.Fprintf(out, "pve-csi version=%s go=%s platform=%s/%s\n",
		Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	cfg, err := LoadConfig(getenv)
	if err != nil {
		return err
	}
	client, err := pveapi.NewClient(cfg.Connection)
	if err != nil {
		return err
	}
	defer client.Close()

	info, err := client.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "PVE version=%s release=%s repoid=%s\n", info.Version, info.Release, info.RepoID)

	resources, err := client.Resources(ctx)
	if err != nil {
		return err
	}

	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Type == resources[j].Type {
			return resources[i].ID < resources[j].ID
		}
		return resources[i].Type < resources[j].Type
	})
	fmt.Fprintf(out, "PVE resources=%d\n", len(resources))
	for _, entry := range resources {
		fmt.Fprintf(out, "%s id=%s status=%s", entry.Type, entry.ID, entry.Status)
		if entry.Name != "" {
			fmt.Fprintf(out, " name=%s", entry.Name)
		}
		fmt.Fprintln(out)
	}
	if cfg.Query.VM.VMID != 0 {
		vm, err := client.QueryVM(ctx, cfg.Query.VM, resources)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "PVE VM vmid=%d node=%s name=%s uuid=%s\n", vm.VMID, vm.Node, vm.Name, vm.UUID)
		var identityChecks []string
		if cfg.Query.VM.ExpectedUUID != "" {
			identityChecks = append(identityChecks, "uuid")
		}
		if cfg.Query.VM.ExpectedName != "" {
			identityChecks = append(identityChecks, "name")
		}
		if len(identityChecks) > 0 {
			fmt.Fprintf(out, "PVE VM identity verified vmid=%d checks=%s\n", vm.VMID, strings.Join(identityChecks, ","))
		}
		if cfg.Query.Storage != "" {
			storage, err := client.Storage(ctx, vm.Node, cfg.Query.Storage)
			if err != nil {
				return err
			}
			const gib = float64(1 << 30)
			fmt.Fprintf(out, "PVE storage=%s node=%s type=%s enabled=%t active=%t total=%.2fGiB used=%.2fGiB available=%.2fGiB content=%s\n",
				storage.Storage, storage.Node, storage.Type, storage.Enabled, storage.Active,
				float64(storage.Total)/gib, float64(storage.Used)/gib, float64(storage.Avail)/gib, storage.Content)
		}
	}
	return nil
}
