package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/pve"
	"pve-csi.local/csi-plugin/internal/volume"
)

func runCheck(ctx context.Context, out, usage io.Writer, getenv func(string) string, args []string) error {
	// Help and rejected arguments must not depend on local credentials or .env.
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprint(usage, `Usage: pve-csi check
Read-only PVE checks; no disks, VMs, permissions or tokens are modified.
Settings come only from .env or environment variables; configuration flags are not accepted.
Required: PVE_ENDPOINT, PVE_TOKEN_ID, PVE_TOKEN_SECRET, PVE_NODE, PVE_OWNER_VMID,
          PVE_STORAGE, PVE_VMIDS (comma-separated Kubernetes VMIDs).
Optional: PVE_CA_FILE (otherwise system trust), PVE_CHECK_TIMEOUT (default 60s),
          PVE_CHECK_INSECURE (default false; true skips TLS verification for check only).
Automatically reads .env in the current directory if present. No shell commands or variable expansion are executed.
Nonempty process environment variables take precedence over .env values.
The report shows the effective endpoint and Token ID, never the Token Secret.
Exit status: 0 when required checks pass; 1 on failure. Warnings describe untested behavior.
`)
		if err != nil {
			return err
		}
		return flag.ErrHelp
	}
	if len(args) != 0 {
		return errors.New("check settings must come from .env or environment variables; use check --help")
	}
	loadedEnv, err := loadCheckEnvFile(getenv)
	if err != nil {
		return err
	}
	getenv = loadedEnv
	var vmids []int
	var vmidLabels []string
	if raw := strings.TrimSpace(getenv("PVE_VMIDS")); raw != "" {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			id, err := strconv.Atoi(value)
			if !ownerVMIDPattern.MatchString(value) || err != nil || !domain.ValidVMID(id) {
				return errors.New("PVE_VMIDS must be comma-separated integers from 100 to 999999999 without empty entries or leading zeros")
			}
			vmids = append(vmids, id)
			vmidLabels = append(vmidLabels, strconv.Itoa(id))
		}
	}
	if len(vmids) == 0 {
		return errors.New("PVE_VMIDS is required; provide at least one Kubernetes VMID")
	}
	timeout := time.Minute
	if raw := strings.TrimSpace(getenv("PVE_CHECK_TIMEOUT")); raw != "" {
		timeout, err = time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return errors.New("PVE_CHECK_TIMEOUT must be a positive duration such as 60s or 2m")
		}
	}
	insecure := false
	if raw := strings.TrimSpace(getenv("PVE_CHECK_INSECURE")); raw != "" {
		insecure, err = strconv.ParseBool(raw)
		if err != nil {
			return errors.New("PVE_CHECK_INSECURE must be true or false")
		}
	}
	cfg, err := loadPVEConfig(getenv)
	if err != nil {
		return err
	}
	options := volume.CheckOptions{
		Node: cfg.Scope.Node, Storage: strings.TrimSpace(getenv("PVE_STORAGE")),
		OwnerVMID: cfg.Scope.OwnerVMID, VMIDs: vmids,
	}
	if err := options.Validate(); err != nil {
		return fmt.Errorf("invalid check scope (PVE_NODE, PVE_STORAGE, PVE_OWNER_VMID, PVE_VMIDS): %w", err)
	}
	if _, err := fmt.Fprintf(out, "Read-only PVE checks: endpoint=%q token_id=%q\n", cfg.Connection.Endpoint, cfg.Connection.TokenID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Scope: node=%q storage=%q owner_vmid=%d vmids=%s\n", options.Node, options.Storage, options.OwnerVMID, strings.Join(vmidLabels, ",")); err != nil {
		return err
	}
	client, err := pve.NewCheckClient(cfg.Connection, insecure)
	if err != nil {
		// Only CA errors remain after validation; keep file paths and contents private.
		return errors.New("cannot initialize PVE TLS trust; check that PVE_CA_FILE is readable and contains PEM CA certificates")
	}
	defer client.Close()
	gateway, err := pve.NewCheckGateway(client)
	if err != nil {
		return errors.New("cannot initialize PVE check gateway")
	}
	service, err := volume.NewCheckService(gateway)
	if err != nil {
		return errors.New("cannot initialize PVE check service")
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	results, err := service.Check(checkCtx, options)
	if err != nil {
		return errors.New("PVE check could not complete")
	}
	if insecure {
		results = append(volume.CheckReport{{
			Name: "tls_verification", Target: volume.CheckTarget{Kind: volume.CheckConnectionTarget}, Status: volume.CheckWarn,
			Detail: "TLS certificate verification is disabled for this check; the server identity is not verified",
		}}, results...)
	}
	return printCheckResults(out, results)
}

func printCheckResults(out io.Writer, results volume.CheckReport) error {
	counts := map[string]int{"PASS": 0, "FAIL": 0, "WARN": 0, "SKIP": 0}
	for _, result := range results {
		if _, known := counts[result.Status]; !known {
			return errors.New("PVE check returned an invalid result status")
		}
		counts[result.Status]++
		target, err := formatCheckTarget(result.Target)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "[%s] %s %s: %s\n", result.Status, result.Name, target, result.Detail); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "Results: %d passed, %d failed, %d warnings, %d skipped\n",
		counts["PASS"], counts["FAIL"], counts["WARN"], counts["SKIP"]); err != nil {
		return err
	}
	if !results.Successful() {
		return errors.New("PVE checks did not all pass; see the results above")
	}
	return nil
}

// Target paths are display labels, not backend API routes.
func formatCheckTarget(target volume.CheckTarget) (string, error) {
	switch target.Kind {
	case volume.CheckNodeTarget:
		return "/nodes/" + target.Node, nil
	case volume.CheckInventoryTarget:
		return "/vms", nil
	case volume.CheckVMTarget:
		return "/vms/" + strconv.Itoa(target.VMID), nil
	case volume.CheckStorageTarget:
		return "/storage/" + target.Storage, nil
	case volume.CheckPlacementTarget:
		return target.Node + "/" + target.Storage, nil
	case volume.CheckScopeTarget:
		return target.Node, nil
	case volume.CheckConnectionTarget:
		return "HTTPS", nil
	default:
		return "", errors.New("PVE check returned an invalid result target")
	}
}
