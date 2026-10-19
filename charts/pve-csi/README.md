# PVE CSI Helm chart

Installs one Controller Pod (CSI driver, provisioner, attacher) and one Node Pod (CSI driver, registrar) on each selected Kubernetes node. It does not create PVCs, PVE disks, or API token Secrets.

Requirements: a single PVE host, LVM-thin storage, Linux amd64 VMs with kernel 5.12 or newer, the standard `/var/lib/kubelet` directory, unique SMBIOS UUIDs, VirtIO SCSI, and disk hotplug. The default sidecars require Kubernetes 1.34 or newer. Supported volumes use ext4 and ReadWriteOnce. Complete [PVE setup](../../PVE-SETUP.md) first.

## Configure your installation

Keep your environment settings in your deployment repository. [values.yaml](values.yaml) includes concrete examples: replace the image version, PVE URL, host, reserved owner number, Secret name, and storage ID with your own settings. Other values can keep their defaults. Optional fields explain what leaving them empty means.

```yaml
image:
  tag: v0.1.0
pve:
  endpoint: https://pve.example.com:8006
  node: pve
  ownerVMID: 7777
  existingSecret: pve-csi-pve
  # Set only when PVE uses a CA that the container does not already trust.
  caConfigMap: pve-csi-ca
storageClass:
  create: true
  name: pve-csi
  storage: data-ssd
```

`v0.1.0` is an example: use an image version that CI has successfully published. Names such as `pve-csi-pve` and `pve-csi-ca` refer to resources you must create in the release namespace before the Controller starts.

| Setting | Meaning |
| --- | --- |
| `image.tag` | Required. A Git tag whose image has been published by CI, such as `v0.1.0`. |
| `pve.endpoint` | HTTPS API URL reachable from the Controller. Its hostname or IP must appear in the server certificate. |
| `pve.node` | PVE host name shown in the PVE UI, such as `pve`; not a Kubernetes node name. |
| `pve.ownerVMID` | Reserved disk owner number. It must not be used by an actual VM or LXC container. |
| `pve.existingSecret` | Existing Secret containing `PVE_TOKEN_ID` and `PVE_TOKEN_SECRET`. Do not put token values in public Git repositories. |
| `pve.caConfigMap` | Optional existing ConfigMap containing the CA certificate under `ca.crt`. Empty means use system trust without mounting a CA file. |
| `nodeSelector` | Selects nodes for both workloads. Defaults to Linux amd64. All selected nodes must be VMs on `pve.node`; the selector does not verify their PVE host. |
| `tolerations` | Allows scheduling despite matching node taints. The default permits control-plane nodes; it does not force scheduling there. |
| `imagePullSecrets` | Registry login Secret names. Leave `[]` for public images. A public GitHub repository does not automatically make its GHCR package public. |

The Node Pods do not receive PVE API tokens or CA certificates. Restart the Controller after changing the contents of an existing token Secret or CA ConfigMap.

## Why a CA certificate is needed

The Controller must verify that it is talking to your PVE server before sending its token. The server presents a certificate identifying its hostname or IP. A Certificate Authority (CA) signs that certificate, and the Controller checks the signature against a CA it trusts. TLS also verifies that the server possesses the matching private key.

PVE normally creates its own CA and node certificates. Containers do not automatically trust that private CA. Obtain the public CA certificate from your trusted PVE installation and put it in the referenced ConfigMap as `ca.crt`; do not copy its private key. See [Proxmox certificate management](https://github.com/proxmox/pve-docs/blob/master/certificate-management.adoc).

Trusting the CA does not disable hostname/IP or expiration checks. If a certificate covers only `pve.example.com`, connecting to its IP will still fail certificate verification. If PVE already serves a certificate trusted by the container, leave `pve.caConfigMap` empty. The API token separately determines which operations the Controller may perform.

## StorageClass settings

A StorageClass is a reusable set of disk provisioning instructions. Creating it does not allocate a disk. A PVC selects it by name and requests its own capacity.

```yaml
storageClass:
  create: true
  name: pve-csi
  storage: data-ssd
  reclaimPolicy: Delete
  volumeBindingMode: Immediate
  defaultClass: false
```

| Setting | Meaning |
| --- | --- |
| `create` | `true`: this chart creates the StorageClass. `false`: manage it elsewhere; the chart still installs the CSI driver. |
| `name` | Kubernetes StorageClass name. PVCs use this as `spec.storageClassName`. |
| `storage` | Existing PVE storage ID where new disks are allocated, such as `data-ssd`. Required when `create=true`. |
| `reclaimPolicy` | `Delete`: deleting a PVC eventually removes its dynamically provisioned PV and backing disk after use and cleanup complete. `Retain`: preserve the disk and PV for manual handling. Deleting only a Pod does not delete its PVC. |
| `volumeBindingMode` | `Immediate`: provision/bind when the PVC is created. `WaitForFirstConsumer`: wait for a Pod using the PVC so scheduling requirements can be considered. |
| `defaultClass` | `false`: this class is not offered as a default; explicitly select it in PVCs. `true`: make it a cluster default for PVCs that omit `storageClassName`. |

For example, the relevant PVC fields for a 10 GiB volume are:

```yaml
spec:
  storageClassName: pve-csi
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
```

The chart sets the StorageClass provisioner to `driverName`, fixes the filesystem to ext4, disables expansion, and restricts topology to `pve.node`. LVM-thin is the PVE storage backend; ext4 is the filesystem inside each disk. See [Kubernetes StorageClasses](https://kubernetes.io/docs/concepts/storage/storage-classes/).

## Use with Argo CD and control versions

Add these fields to your existing Application and put your environment values under `source.helm.valuesObject`, or use your existing values-file workflow:

```yaml
spec:
  source:
    repoURL: https://github.com/weichen-lin/pve-csi.git
    targetRevision: v0.1.0
    path: charts/pve-csi
    helm:
      valuesObject:
        image:
          tag: v0.1.0
        # Add your pve and storageClass settings here.
  destination:
    server: https://kubernetes.default.svc
    namespace: pve-csi
  syncPolicy:
    syncOptions:
      - CreateNamespace=true
```

This is an Application fragment; keep your existing metadata and project. The destination above targets the cluster running Argo CD. The Git revision must contain `charts/pve-csi`, and the image tag must already exist.

| Version | Controls |
| --- | --- |
| `source.targetRevision` | Git tag or commit from which Argo CD reads the chart. |
| `image.tag` | CSI application image run by the Pods, using the corresponding published Git tag. |
| `Chart.yaml` → `version` | Chart package version; it does not select the Git revision or application image. |

For a release, push a new Git tag, wait for CI to publish its image with the same tag, update the deployment repository's Git revision and image tag, then sync Argo CD. Keep published release tags unchanged. Pull requests and pushes to main run checks only; image builds and publication run only on tag pushes after checks pass.

The AppProject must allow the required Namespace, ClusterRole, ClusterRoleBinding, CSIDriver, StorageClass, Deployment, DaemonSet, and ServiceAccount resources. The target namespace must permit privileged Node Pods and host mounts. This chart does not create the namespace; create it before any prerequisite Secret or ConfigMap. Do not manage the same installation with both Argo CD and manual `helm upgrade` commands.

## What `.Release.Name` means

Helm fills template expressions before sending YAML to Kubernetes:

```yaml
app.kubernetes.io/instance: {{ .Release.Name | quote }}
```

For an installation named `pve-csi`, this becomes:

```yaml
app.kubernetes.io/instance: "pve-csi"
```

`.Release.Name` is the installation name, not a version. `quote` wraps it in quotes as a YAML string. With Argo CD, the Helm release name defaults to the Application name. The label groups resources belonging to that installation; this chart also uses it with a component label to select the Controller or Node Pods. Keep the installation name stable during upgrades. See [Helm built-in objects](https://helm.sh/docs/chart_template_guide/builtin_objects/) and [Argo CD Helm release names](https://argo-cd.readthedocs.io/en/stable/user-guide/helm/#helm-release-name).

## Sidecar versions

The chart carries forward the version numbers from this repository's previous deployment manifests. These are independently versioned upstream programs, not versions of the PVE CSI application.

| Component | Default | Role | Upstream minimum Kubernetes |
| --- | --- | --- | --- |
| [external-attacher](https://github.com/kubernetes-csi/external-attacher/releases/tag/v4.13.0) | `v4.13.0` | Requests disk attachment and detachment through the CSI Controller. | 1.17 |
| [external-provisioner](https://github.com/kubernetes-csi/external-provisioner/releases/tag/v6.4.0) | `v6.4.0` | Requests volume creation/deletion and manages dynamically provisioned PVs. | 1.34 |
| [node-driver-registrar](https://github.com/kubernetes-csi/node-driver-registrar/releases/tag/v2.18.0) | `v2.18.0` | Registers the local CSI Node service with kubelet. | 1.13 |

Upstream marks all three releases GA. The provisioner supports CSI 1.0–1.13; this project depends on CSI 1.13. The default combination therefore requires at least Kubernetes 1.34. This is an upstream compatibility floor, not a claim that every Kubernetes version has been tested with this driver.

Use explicit versions, check upstream release notes and compatibility requirements before changing them, and verify provisioning, attachment, mounting, and cleanup on the target cluster. Rendering the chart alone does not test those operations. Override the full image references through `sidecars.attacher`, `sidecars.provisioner`, and `sidecars.registrar` when needed.

## Validate and maintain

```sh
helm lint charts/pve-csi --strict --kube-version 1.34.0 -f charts/pve-csi/ci/values.yaml
# Render your settings locally without contacting the cluster.
helm template pve-csi charts/pve-csi --namespace pve-csi --kube-version 1.34.0 -f my-values.yaml
```

For local rendering, set `--kube-version` to the target Kubernetes version; these examples use the minimum supported release. Helm rejects versions below 1.34. The values schema rejects invalid PVE host names and invalid StorageClass, Secret, and CA ConfigMap names before deployment.

`ci/values.yaml` contains fictional inputs for offline checks, not deployment settings. Empty or invalid image tags fail validation. The example defaults can render successfully; rendering does not verify that your image, PVE host, storage, or Secret actually exists.

Install each `driverName` only once per cluster. Do not change the driver name or disk owner VMID while its PVs exist. StorageClass provisioning fields cannot be edited in place: create a differently named class when changing storage pools; existing PVs keep their settings. Multiple Controller replicas, custom kubelet paths, expansion, and snapshots are not supported.

Before uninstalling, handle the consuming Pods and PVCs and confirm unmounting, detachment, and intended disk deletion have completed. Upgrades do not require deleting PVCs. Do not use Argo CD Force/Replace to recreate storage resources.
