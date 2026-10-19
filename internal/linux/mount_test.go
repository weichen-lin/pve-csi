package linux

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestDeviceSelectionRefusesAmbiguityAndPartitions(t *testing.T) {
	disk := device{Name: "sdb", Type: "disk", Serial: "expected", Number: "8:16"}
	for _, tc := range []struct {
		name    string
		devices []device
		want    error
	}{
		{"missing", nil, volume.ErrDeviceNotFound},
		{"duplicate", []device{disk, disk}, volume.ErrUnsafeMount},
		{"partitioned", []device{{Name: "sda", Type: "disk", Serial: "expected", Number: "8:0", Children: []device{{Name: "sda1", Type: "part"}}}}, volume.ErrUnsafeMount},
		{"not a disk", []device{{Name: "sdb1", Type: "part", Serial: "expected", Number: "8:17"}}, volume.ErrUnsafeMount},
		{"device path injection", []device{{Name: "../sda", Type: "disk", Serial: "expected", Number: "8:0"}}, volume.ErrUnsafeMount},
		{"one managed disk", []device{disk}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := selectDevice(tc.devices, "expected")
			if !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
}

func TestSCSISerialWithoutUdev(t *testing.T) {
	raw := append([]byte{0, 0x80, 0, 31}, []byte("csi390c703a2952c24b0b16437ae8f1")...)
	serial, err := scsiSerial(raw)
	if err != nil || serial != "csi390c703a2952c24b0b16437ae8f1" {
		t.Fatal(serial, err)
	}
	for _, bad := range [][]byte{{}, raw[:10], append([]byte{0, 0x83, 0, 31}, raw[4:]...)} {
		if _, err := scsiSerial(bad); !errors.Is(err, volume.ErrUnsafeMount) {
			t.Fatal("malformed VPD accepted")
		}
	}
}
func TestMountIdentityAndStackedMounts(t *testing.T) {
	list, err := parseMounts("10 1 8:16 / /stage rw,relatime - ext4 /dev/sdb rw\n11 1 8:16 / /pod ro,relatime - ext4 /dev/sdb rw")
	if err != nil {
		t.Fatal(err)
	}
	m, found, err := mountAt(list, "/pod")
	if err != nil || !found || !readOnly(m) || !matches(m, device{Number: "8:16"}) {
		t.Fatal("bind mount not recognized")
	}
	if matches(m, device{Number: "8:0"}) {
		t.Fatal("wrong disk accepted")
	}
	m.Root = "/subdirectory"
	if matches(m, device{Number: "8:16"}) {
		t.Fatal("partial filesystem accepted")
	}
	list = append(list, list[0])
	if _, _, err := mountAt(list, "/stage"); !errors.Is(err, volume.ErrUnsafeMount) {
		t.Fatal("stacked mount accepted")
	}
}
func TestPathsRejectSymlinksAndWrongVolumes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := safeDirectory(filepath.Join(dir, "link", "child"), true); !errors.Is(err, volume.ErrInvalidMountPath) {
		t.Fatal(err)
	}
	req, err := domain.NewRequest("one", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHandle(req.Handle())
	g := NewMountGateway("example.csi")
	for _, path := range []string{"/", "/var/lib/kubelet/plugins/other/globalmount", g.stagingPath(id) + "/../globalmount"} {
		if err := g.stagePath(id, path, false); !errors.Is(err, volume.ErrInvalidMountPath) {
			t.Fatal(path, err)
		}
	}
	for _, path := range []string{"/etc", "/var/lib/kubelet/pods/../../etc", "/var/lib/kubelet/pods/id/volumes/kubernetes.io~csi/pv/mount"} {
		if err := g.targetPath(path, false); !errors.Is(err, volume.ErrInvalidMountPath) {
			t.Fatal(path, err)
		}
	}
	if filesystemUUID(id) == "" || id.Serial() == "" {
		t.Fatal("missing stable identity")
	}
}

func TestFormattingRequiresExplicitlyEmptySignatures(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"signatures":null}`, `{"signatures":{}}`, `{"signatures":[{"type":"ext4"}]}`, `{"signatures":`} {
		if err := checkBlankSignatures([]byte(raw)); err == nil {
			t.Errorf("unsafe format authorized by %s", raw)
		}
	}
	if err := checkBlankSignatures([]byte(`{"signatures":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestExistingPublishMustMatchReadonly(t *testing.T) {
	d := device{Number: "8:16"}
	for _, tc := range []struct {
		existing  string
		requested bool
		want      error
	}{
		{"rw", false, nil}, {"ro", true, nil},
		{"rw", true, volume.ErrMountConflict}, {"ro", false, volume.ErrMountConflict},
	} {
		m := mount{Number: d.Number, FSType: "ext4", Root: "/", Options: tc.existing}
		if err := checkPublishedMount(m, d, tc.requested); !errors.Is(err, tc.want) {
			t.Fatal(err)
		}
		m.Number = "8:0"
		if err := checkPublishedMount(m, d, tc.requested); !errors.Is(err, volume.ErrUnsafeMount) {
			t.Fatal("accepted another disk")
		}
	}
}
