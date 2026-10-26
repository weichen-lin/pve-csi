package pve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestCheckGatewayRequiresInitializedClient(t *testing.T) {
	for _, client := range []*Client{nil, {}} {
		if gateway, err := NewCheckGateway(client); err == nil || gateway != nil {
			t.Fatal("uninitialized client accepted")
		}
	}
}

func TestCheckGatewayReusesApplicationErrorDetails(t *testing.T) {
	for _, cause := range []error{
		context.Canceled, context.DeadlineExceeded,
		domain.ErrNodeAmbiguous, domain.ErrNodeNotFound, domain.ErrInvalidInventory,
		domain.ErrAttachmentConflict, domain.ErrOwnerInUse, domain.ErrPoolDisabled,
		domain.ErrPoolInactive, domain.ErrStorage, domain.ErrCapacity, volume.ErrBusy,
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("%s: %w", checkTestSecret, cause)
			want := volume.CheckFailureDetail(wrapped)
			err := normalizeCheckError(wrapped)
			var failure *volume.CheckFailure
			if !errors.As(err, &failure) || !errors.Is(err, cause) {
				t.Fatalf("normalized error lost its classification or cause: %v", err)
			}
			if want == "" || failure.Hint() != want || volume.CheckFailureDetail(err) != want {
				t.Fatalf("adapter and application disagree: adapter=%q application=%q", failure.Hint(), want)
			}
			if strings.Contains(want, checkTestSecret) {
				t.Fatal("shared diagnostic exposed underlying error text")
			}
		})
	}
}

func TestCheckGatewayNormalizesEveryPortFailure(t *testing.T) {
	id, err := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	placement := domain.Placement{Node: "pve", Storage: "pool"}
	vm := domain.VirtualMachine{Node: "pve", VMID: 100, ID: id}
	methods := []struct {
		name string
		call func(*CheckGateway) error
	}{
		{"task access", func(g *CheckGateway) error { _, err := g.TaskAccess(ctx, "pve"); return err }},
		{"inventory access", func(g *CheckGateway) error { _, err := g.InventoryAccess(ctx); return err }},
		{"visible nodes", func(g *CheckGateway) error { _, err := g.VisibleNodes(ctx); return err }},
		{"node status", func(g *CheckGateway) error { _, err := g.NodeStatus(ctx, "pve"); return err }},
		{"active tasks", func(g *CheckGateway) error { _, err := g.ActiveTasks(ctx, "pve"); return err }},
		{"owner unused", func(g *CheckGateway) error { _, err := g.OwnerUnused(ctx, 7777); return err }},
		{"storage read", func(g *CheckGateway) error { _, err := g.StorageReadAccess(ctx, placement, 7777); return err }},
		{"storage allocation", func(g *CheckGateway) error { _, err := g.StorageAllocationAccess(ctx, placement); return err }},
		{"storage deletion", func(g *CheckGateway) error { _, err := g.StorageDeletionAccess(ctx, placement); return err }},
		{"inspect pool", func(g *CheckGateway) error { _, err := g.InspectPool(ctx, placement, 7777); return err }},
		{"image count", func(g *CheckGateway) error { _, err := g.ImageCount(ctx, placement, 7777); return err }},
		{"list VMs", func(g *CheckGateway) error { _, err := g.ListVMs(ctx, "pve"); return err }},
		{"VM identities", func(g *CheckGateway) error {
			_, err := g.VMIdentities(ctx, "pve", []volume.CheckVM{{VMID: 100}})
			return err
		}},
		{"VM read", func(g *CheckGateway) error { _, err := g.VMReadAccess(ctx, 100); return err }},
		{"VM disk", func(g *CheckGateway) error { _, err := g.VMDiskAccess(ctx, 100); return err }},
		{"VM identity", func(g *CheckGateway) error { _, err := g.VMIdentity(ctx, "pve", 100); return err }},
		{"attachment readiness", func(g *CheckGateway) error { _, err := g.AttachmentReadiness(ctx, vm); return err }},
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newCheckFixture()
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				w.WriteHeader(status)
				io.WriteString(w, checkTestSecret)
				return true
			}
			gateway, err := NewCheckGateway(f.serve(t))
			if err != nil {
				t.Fatal(err)
			}
			for _, method := range methods {
				t.Run(method.name, func(t *testing.T) {
					err := method.call(gateway)
					var failure *volume.CheckFailure
					var response *HTTPError
					if !errors.As(err, &failure) || !errors.As(err, &response) || response.StatusCode != status {
						t.Fatalf("missing application classification or HTTP cause: %v", err)
					}
					if failure.Terminal() != (status != http.StatusForbidden) || failure.Hint() == "" {
						t.Fatalf("unexpected failure classification: terminal=%t hint=%q", failure.Terminal(), failure.Hint())
					}
					if strings.Contains(failure.Hint(), checkTestSecret) || strings.Contains(err.Error(), checkTestSecret) {
						t.Fatal("server body leaked through observation error")
					}
				})
			}
		})
	}
}

func TestCheckGatewayPreservesSafeCausesAndHints(t *testing.T) {
	for _, tc := range []struct {
		name, hint string
		cause      error
		terminal   bool
	}{
		{"cancel", "canceled", context.Canceled, true},
		{"deadline", "timed out", context.DeadlineExceeded, true},
		{"missing UUID", "no SMBIOS UUID", errMissingVMUUID, false},
		{"invalid inventory", "UUIDs", domain.ErrInvalidInventory, false},
		{"attachment conflict", "identity or configuration changed", domain.ErrAttachmentConflict, false},
		{"busy", "locked", volume.ErrBusy, false},
		{"unsupported storage", "does not support CSI volumes", domain.ErrStorage, false},
		{"permission", "VM.Config.Disk on /vms/100", missingPrivilege("/vms/100", "VM.Config.Disk"), false},
		{"TLS", "TLS certificate verification failed", &url.Error{Op: "Get", URL: "https://" + checkTestSecret, Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, true},
		{"connection", "cannot reach PVE", &url.Error{Op: "Get", URL: "https://" + checkTestSecret, Err: errors.New(checkTestSecret)}, true},
		{"invalid response", "invalid or unexpected response", errors.New(checkTestSecret), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := normalizeCheckError(fmt.Errorf("adapter context: %w", tc.cause))
			var failure *volume.CheckFailure
			if !errors.As(err, &failure) || !errors.Is(err, tc.cause) {
				t.Fatalf("underlying cause was lost: %v", err)
			}
			if failure.Terminal() != tc.terminal || !strings.Contains(failure.Hint(), tc.hint) {
				t.Fatalf("wrong classification: terminal=%t hint=%q", failure.Terminal(), failure.Hint())
			}
			if strings.Contains(failure.Hint(), checkTestSecret) || strings.Contains(err.Error(), checkTestSecret) {
				t.Fatal("unsafe error text escaped adapter")
			}
		})
	}
}

func TestCheckGatewayHTTPTimeoutDoesNotSuggestIncreasingCheckTimeout(t *testing.T) {
	f := newCheckFixture()
	f.override = func(_ http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	}
	client := f.serve(t)
	client.httpClient.Timeout = 50 * time.Millisecond
	gateway, err := NewCheckGateway(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = gateway.TaskAccess(ctx, "pve")
	var failure *volume.CheckFailure
	var requestError *url.Error
	if !errors.As(err, &failure) || !errors.As(err, &requestError) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTP timeout lost its classification or cause: %v", err)
	}
	if ctx.Err() != nil || !failure.Terminal() {
		t.Fatalf("HTTP timeout must stop checks while the outer context remains live: context=%v terminal=%t", ctx.Err(), failure.Terminal())
	}
	detail := volume.CheckFailureDetail(err)
	if detail != failure.Hint() || !strings.Contains(detail, "timed out") || strings.Contains(detail, "increase the check timeout") {
		t.Fatalf("HTTP timeout gave misleading guidance: %q", detail)
	}
	for _, private := range []string{checkTestSecret, client.config.Endpoint, requestError.Error()} {
		if strings.Contains(detail, private) || strings.Contains(err.Error(), private) {
			t.Fatal("HTTP timeout exposed credentials or raw transport details")
		}
	}
}

func TestCheckGatewayVMIdentitiesRejectsRevokedInventoryAccess(t *testing.T) {
	f := newCheckFixture()
	permissionReads := 0
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api2/json/access/permissions" || r.URL.Query().Get("path") != "/vms" {
			return false
		}
		permissionReads++
		if permissionReads == 2 {
			io.WriteString(w, `{"data":{"/vms":{}}}`)
			return true
		}
		return false
	}
	gateway, err := NewCheckGateway(f.serve(t))
	if err != nil {
		t.Fatal(err)
	}
	machines, err := gateway.VMIdentities(context.Background(), "pve", []volume.CheckVM{{VMID: 100}, {VMID: 101}})
	var failure *volume.CheckFailure
	if machines != nil || !errors.As(err, &failure) || failure.Terminal() || !strings.Contains(failure.Hint(), "VM.Audit with propagation") {
		t.Fatalf("revoked access returned a usable inventory: machines=%v error=%v", machines, err)
	}
	if permissionReads != 2 {
		t.Fatalf("inventory access was not checked before and after: %d", permissionReads)
	}
}
