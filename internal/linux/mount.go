// Package linux implements guest device and mount operations.
package linux

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

type hostOperations struct {
	run         func(context.Context, string, ...string) ([]byte, error)
	device      func(context.Context, domain.Identity) (device, error)
	filesystem  func(context.Context, device, domain.Identity) (bool, error)
	mounts      func() ([]mount, error)
	directory   func(string, bool) error
	readDir     func(string) ([]os.DirEntry, error)
	removeEmpty func(string) error
	openTree    func(string) (mountTree, error)
}

type MountGateway struct {
	driver string
	hostOperations
}

func NewMountGateway(driver string) *MountGateway {
	return &MountGateway{driver: driver, hostOperations: hostOperations{
		run: command, device: findDevice, filesystem: inspectFilesystem,
		mounts: mounts, directory: safeDirectory, readDir: os.ReadDir,
		removeEmpty: removeEmpty, openTree: openDetachedMount,
	}}
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	started := time.Now()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	attrs := []any{"command", name, "duration_ms", time.Since(started).Milliseconds()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			attrs = append(attrs, "exit_code", exit.ExitCode())
		}
		if ctx.Err() != nil {
			attrs = append(attrs, "canceled", true)
		}
		// Arguments and stdout/stderr can contain untrusted data. Keep them out.
		// blkid exit 2 with no output is the normal blank-disk observation.
		if name != "blkid" || exit == nil || exit.ExitCode() != 2 || len(out) != 0 {
			slog.ErrorContext(ctx, "node_command_failed", attrs...)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return out, volume.NewBackendError(volume.FailureInternal, name+" failed; inspect node device and mount state", err)
	}
	slog.InfoContext(ctx, "node_command_finished", append(attrs, "exit_code", 0)...)
	return out, nil
}

func filesystemUUID(id domain.Identity) string {
	hash := sha256.Sum256([]byte(id.ID()))
	return fmt.Sprintf("%x-%x-%x-%x-%x", hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}

func (g *MountGateway) stagingPath(id domain.Identity) string {
	hash := sha256.Sum256([]byte(id.ID()))
	return fmt.Sprintf("/var/lib/kubelet/plugins/kubernetes.io/csi/%s/%x/globalmount", g.driver, hash)
}

var targetPattern = regexp.MustCompile(`^/var/lib/kubelet/pods/[0-9a-f-]{36}/volumes/kubernetes\.io~csi/[a-zA-Z0-9_.-]+/mount$`)
var deviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func safeDirectory(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return volume.ErrInvalidMountPath
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return nil
			}
			if err = os.Mkdir(current, 0750); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return volume.ErrInvalidMountPath
		}
	}
	return nil
}
func (g *MountGateway) stagePath(id domain.Identity, path string, create bool) error {
	if path != g.stagingPath(id) {
		return volume.ErrInvalidMountPath
	}
	return g.directory(path, create)
}
func (g *MountGateway) targetPath(path string, create bool) error {
	if !targetPattern.MatchString(path) {
		return volume.ErrInvalidMountPath
	}
	return g.directory(path, create)
}

type device struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Serial   string   `json:"serial"`
	Number   string   `json:"maj:min"`
	Children []device `json:"children"`
}

func selectDevice(devices []device, serial string) (device, error) {
	var matches []device
	var visit func([]device)
	visit = func(items []device) {
		for _, d := range items {
			if strings.TrimSpace(d.Serial) == serial {
				matches = append(matches, d)
			}
			visit(d.Children)
		}
	}
	visit(devices)
	if len(matches) == 0 {
		return device{}, volume.ErrDeviceNotFound
	}
	if len(matches) != 1 {
		return device{}, volume.ErrUnsafeMount
	}
	d := matches[0]
	if d.Type != "disk" || len(d.Children) != 0 || d.Number == "" || !deviceNamePattern.MatchString(d.Name) {
		return device{}, volume.ErrUnsafeMount
	}
	return d, nil
}
func findDevice(ctx context.Context, id domain.Identity) (device, error) {
	out, err := command(ctx, "lsblk", "--json", "--output", "NAME,TYPE,MAJ:MIN")
	if err != nil {
		return device{}, err
	}
	var list struct {
		Devices []device `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return device{}, err
	}
	// lsblk SERIAL depends on the host udev database, absent in containers.
	// Read the kernel's SCSI unit-serial VPD page instead of mounting /run/udev.
	for i := range list.Devices {
		d := &list.Devices[i]
		if d.Type != "disk" {
			continue
		}
		if filepath.Base(d.Name) != d.Name {
			return device{}, volume.ErrUnsafeMount
		}
		raw, err := os.ReadFile("/sys/class/block/" + d.Name + "/device/vpd_pg80")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return device{}, err
		}
		d.Serial, err = scsiSerial(raw)
		if err != nil {
			return device{}, err
		}
	}
	d, err := selectDevice(list.Devices, id.Serial())
	if err != nil {
		return device{}, err
	}
	info, err := os.Lstat("/dev/" + d.Name)
	if err != nil {
		return device{}, err
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 || info.Mode()&os.ModeSymlink != 0 {
		return device{}, volume.ErrUnsafeMount
	}
	holders, err := os.ReadDir("/sys/class/block/" + d.Name + "/holders")
	if err != nil {
		return device{}, err
	}
	if len(holders) != 0 {
		return device{}, volume.ErrUnsafeMount
	}
	return d, nil
}

func scsiSerial(raw []byte) (string, error) {
	if len(raw) < 4 || raw[1] != 0x80 || int(binary.BigEndian.Uint16(raw[2:4])) != len(raw)-4 {
		return "", volume.ErrUnsafeMount
	}
	return strings.TrimSpace(string(raw[4:])), nil
}

type mount struct{ Number, Root, Path, Options, FSType string }

func parseMounts(data string) ([]mount, error) {
	var result []mount
	decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			return nil, volume.ErrUnsafeMount
		}
		a, b := strings.Fields(before), strings.Fields(after)
		if len(a) < 6 || len(b) < 3 {
			return nil, volume.ErrUnsafeMount
		}
		result = append(result, mount{Number: a[2], Root: decode.Replace(a[3]), Path: decode.Replace(a[4]), Options: a[5], FSType: b[0]})
	}
	return result, nil
}
func mounts() ([]mount, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return parseMounts(string(data))
}
func mountAt(list []mount, path string) (mount, bool, error) {
	var found mount
	n := 0
	for _, m := range list {
		if m.Path == path {
			found = m
			n++
		}
	}
	if n > 1 {
		return mount{}, false, volume.ErrUnsafeMount
	}
	return found, n == 1, nil
}
func matches(m mount, d device) bool {
	return m.Number == d.Number && m.FSType == "ext4" && m.Root == "/"
}
func readOnly(m mount) bool { return strings.Contains(","+m.Options+",", ",ro,") }

func inspectFilesystem(ctx context.Context, d device, id domain.Identity) (bool, error) {
	out, err := command(ctx, "blkid", "-p", "-o", "export", "--", "/dev/"+d.Name)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 && len(out) == 0 {
			return false, nil
		}
		return false, err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	if fields["TYPE"] != "ext4" || fields["UUID"] != filesystemUUID(id) || fields["PTTYPE"] != "" {
		return false, volume.ErrUnsafeMount
	}
	return true, nil
}

func checkBlankSignatures(out []byte) error {
	var signatures struct {
		Signatures []json.RawMessage `json:"signatures"`
	}
	if err := json.Unmarshal(out, &signatures); err != nil {
		return err
	}
	if signatures.Signatures == nil || len(signatures.Signatures) != 0 {
		return volume.ErrUnsafeMount
	}
	return nil
}

func (g *MountGateway) Stage(ctx context.Context, id domain.Identity, path string) error {
	if err := g.stagePath(id, path, false); err != nil {
		return err
	}
	d, err := g.device(ctx, id)
	if err != nil {
		return err
	}
	list, err := g.mounts()
	if err != nil {
		return err
	}
	existing, found, err := mountAt(list, path)
	if err != nil {
		return err
	}
	formatted, err := g.filesystem(ctx, d, id)
	if err != nil {
		return err
	}
	if found {
		if !formatted || !matches(existing, d) || readOnly(existing) {
			return volume.ErrUnsafeMount
		}
		return nil
	}
	for _, m := range list {
		if m.Number == d.Number {
			return volume.ErrMountInUse
		}
	}
	if err := g.stagePath(id, path, true); err != nil {
		return err
	}
	entries, err := g.readDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return volume.ErrUnsafeMount
	}
	if !formatted {
		out, err := g.run(ctx, "wipefs", "--no-act", "--json", "--output", "TYPE", "--", "/dev/"+d.Name)
		if err != nil {
			return err
		}
		if err := checkBlankSignatures(out); err != nil {
			return err
		}
		if _, err := g.run(ctx, "mkfs.ext4", "-q", "-U", filesystemUUID(id), "-E", "lazy_itable_init=0,lazy_journal_init=0", "/dev/"+d.Name); err != nil {
			return err
		}
	}
	if ready, err := g.filesystem(ctx, d, id); err != nil {
		return err
	} else if !ready {
		return volume.ErrUnsafeMount
	}
	if _, err := g.run(ctx, "mount", "-t", "ext4", "-o", "rw", "--", "/dev/"+d.Name, path); err != nil {
		return err
	}
	return g.verifyMount(path, d, false)
}
func (g *MountGateway) verifyMount(path string, d device, ro bool) error {
	list, err := g.mounts()
	if err != nil {
		return err
	}
	m, found, err := mountAt(list, path)
	if err != nil {
		return err
	}
	if !found || !matches(m, d) || readOnly(m) != ro {
		return volume.ErrUnsafeMount
	}
	return nil
}
func (g *MountGateway) Publish(ctx context.Context, id domain.Identity, stage, target string, ro bool) error {
	if err := g.stagePath(id, stage, false); err != nil {
		return err
	}
	if err := g.targetPath(target, false); err != nil {
		return err
	}
	d, err := g.device(ctx, id)
	if err != nil {
		return err
	}
	if ready, err := g.filesystem(ctx, d, id); err != nil {
		return err
	} else if !ready {
		return volume.ErrUnsafeMount
	}
	if err := g.verifyMount(stage, d, false); err != nil {
		return err
	}
	list, err := g.mounts()
	if err != nil {
		return err
	}
	m, found, err := mountAt(list, target)
	if err != nil {
		return err
	}
	if found {
		return checkPublishedMount(m, d, ro)
	}
	if err := g.targetPath(target, true); err != nil {
		return err
	}
	entries, err := g.readDir(target)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return volume.ErrUnsafeMount
	}
	if err := bindMount(ctx, g.openTree, stage, target, ro); err != nil {
		return err
	}
	return g.verifyMount(target, d, ro)
}

func checkPublishedMount(m mount, d device, ro bool) error {
	if !matches(m, d) {
		return volume.ErrUnsafeMount
	}
	if readOnly(m) != ro {
		return volume.ErrMountConflict
	}
	return nil
}

// A detached clone is invisible to the Pod until it has its final access mode.
// On process death before Publish the kernel closes the fd and discards it;
// after Publish the target is already complete and retries only verify it.
type mountTree interface {
	ReadOnly() error
	Publish(string) error
	Close() error
}

func bindMount(ctx context.Context, open func(string) (mountTree, error), stage, target string, ro bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tree, err := open(stage)
	if err != nil {
		return mountAPIError("clone_mount", err)
	}
	defer tree.Close()
	if ro {
		if err := tree.ReadOnly(); err != nil {
			return mountAPIError("set_mount_readonly", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tree.Publish(target); err != nil {
		return mountAPIError("publish_mount", err)
	}
	return nil
}

func mountAPIError(step string, err error) error {
	attrs := []any{"step", step}
	var errno unix.Errno
	if errors.As(err, &errno) {
		attrs = append(attrs, "errno", int(errno))
	}
	slog.Error("node_mount_failed", attrs...)
	kind := volume.FailureInternal
	hint := "Linux mount operation failed; inspect node privileges and mount state"
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		kind = volume.FailurePermissionDenied
		hint = "node lacks mount permission; check privileged mode and kubelet host mount access"
	} else if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		kind = volume.FailurePrecondition
		hint = "node requires Linux 5.12 or newer with open_tree, mount_setattr and move_mount support"
	}
	return volume.AtStep(step, "mount", volume.NewBackendError(kind, hint, err))
}
func (g *MountGateway) Unpublish(ctx context.Context, id domain.Identity, path string) error {
	if err := g.targetPath(path, false); err != nil {
		return err
	}
	return g.unmount(ctx, id, path, false)
}
func (g *MountGateway) Unstage(ctx context.Context, id domain.Identity, path string) error {
	if err := g.stagePath(id, path, false); err != nil {
		return err
	}
	return g.unmount(ctx, id, path, true)
}
func (g *MountGateway) unmount(ctx context.Context, id domain.Identity, path string, stage bool) error {
	list, err := g.mounts()
	if err != nil {
		return err
	}
	m, found, err := mountAt(list, path)
	if err != nil {
		return err
	}
	if !found {
		return g.removeEmpty(path)
	}
	d, err := g.device(ctx, id)
	if err != nil {
		return err
	}
	if !matches(m, d) {
		return volume.ErrUnsafeMount
	}
	formatted, err := g.filesystem(ctx, d, id)
	if err != nil {
		return err
	}
	if !formatted {
		return volume.ErrUnsafeMount
	}
	if stage {
		for _, other := range list {
			if other.Number == d.Number && other.Path != path {
				return volume.ErrMountInUse
			}
		}
	}
	if _, err := g.run(ctx, "umount", "--", path); err != nil {
		return err
	}
	list, err = g.mounts()
	if err != nil {
		return err
	}
	_, found, err = mountAt(list, path)
	if err != nil {
		return err
	}
	if found {
		return volume.ErrMountInUse
	}
	return g.removeEmpty(path)
}
func removeEmpty(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
