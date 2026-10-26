# PVE CSI Helm chart

Installs one Controller Pod (CSI driver, provisioner, attacher) and one Node Pod (CSI driver, registrar) on each selected Kubernetes node. It does not create PVCs, PVE disks, or API token Secrets.

Requirements: a single PVE host, LVM-thin storage, Linux amd64 VMs with kernel 5.12 or newer, the standard `/var/lib/kubelet` directory, unique SMBIOS UUIDs, VirtIO SCSI, and disk hotplug. The default sidecars require Kubernetes 1.34 or newer. Supported volumes use ext4 and ReadWriteOnce. Complete [PVE setup](../../PVE-SETUP.md) first.

Before deploying, run the read-only [`pve-csi check`](../../CHECK.md) locally with the intended CSI token, storage ID, and Kubernetes VMIDs to check PVE permissions and readiness. After deploying, the same check can run inside the cluster on demand; see [On-demand PVE check](#on-demand-pve-check).

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

## Install and control versions

Use a checkout of the Git tag whose image CI has published, and save your installation settings in `my-values.yaml`, including the matching `image.tag`. Create the target namespace before creating the token Secret and optional CA ConfigMap referenced by those settings:

```sh
kubectl create namespace pve-csi
```

The namespace must permit privileged Node Pods and host mounts. After preparing the Secret and any ConfigMap, run Helm from the repository root:

```sh
helm upgrade --install pve-csi charts/pve-csi --namespace pve-csi -f my-values.yaml
```

This installs or upgrades the `pve-csi` release. The chart does not create the namespace, token Secret, or CA ConfigMap.

| Version | Controls |
| --- | --- |
| Git checkout | The chart templates and defaults used by the Helm command. |
| `image.tag` | CSI application image run by the Pods, using the corresponding published Git tag. |
| `Chart.yaml` → `version` | Chart package version; it does not select the Git revision or application image. |

For a release, push a new Git tag and wait for CI to publish its image with the same tag. Deploy from that checkout, update `image.tag` in your values file, and rerun the Helm command. Keep published release tags unchanged. Pull requests and pushes to main run checks only; image builds and publication run only on tag pushes after checks pass.

## On-demand PVE check

Set `check.enabled: true` to install a suspended CronJob. It never runs on a schedule; it only stores the image, PVE settings, token Secret reference, and CA settings so that you can start a one-off read-only [`pve-csi check`](../../CHECK.md) from inside the cluster. Its name is `<release>-check` for release names up to 46 characters; longer names use a shortened prefix and a hash to stay within the CronJob's 52-character limit.

```yaml
check:
  enabled: true
  vmids: [100, 101]
```

| Setting | Meaning |
| --- | --- |
| `check.enabled` | Installs the suspended CronJob. Default `false`. |
| `check.vmids` | Required when enabled. PVE VMIDs of the Kubernetes node VMs. Update this list when node VMs are added or replaced; an outdated list only fails the check, never the CSI driver. |
| `check.timeout` | Upper bound for the whole check, such as `60s` or `2m`. |
| `check.ttlSecondsAfterFinished` | Seconds after completion until a Job is eligible for TTL cleanup. Default `86400` (one day); history cleanup can delete it sooner. |

The check uses `pve.endpoint`, `pve.node`, `pve.ownerVMID`, `pve.existingSecret`, `pve.caConfigMap`, and `storageClass.storage`, so `storageClass.storage` must be set even when `storageClass.create` is false. The check Pod does not receive a Kubernetes API token.

For the `pve-csi` release, start a check with `kubectl`, using a new Job name each time:

```sh
kubectl -n pve-csi create job pve-csi-check-001 --from=cronjob/pve-csi-check
kubectl -n pve-csi logs -f job/pve-csi-check-001
```

For another release, find its CronJob by labels, replacing `pve-csi` in the instance label with your release name:

```sh
kubectl -n pve-csi get cronjob -l 'app.kubernetes.io/instance=pve-csi,app.kubernetes.io/component=check' -o jsonpath='{.items[0].metadata.name}{"\n"}'
```

Use the returned name after `--from=cronjob/`. Each Job runs once without retries and succeeds only when all required checks pass. A failed check does not affect the running Controller or Node Pods.

The CronJob retains at most one successful and one failed Job. Completing another check with the same outcome can delete the earlier Job and its Pod logs before its TTL expires. TTL makes a finished Job eligible for cleanup; it guarantees neither a minimum retention period nor an exact deletion time.

## What `.Release.Name` means

Helm fills template expressions before sending YAML to Kubernetes:

```yaml
app.kubernetes.io/instance: {{ .Release.Name | quote }}
```

For an installation named `pve-csi`, this becomes:

```yaml
app.kubernetes.io/instance: "pve-csi"
```

`.Release.Name` is the installation name passed to Helm, such as `pve-csi` in the install command above. `quote` wraps it in quotes as a YAML string. The label groups resources belonging to that installation; this chart also uses it with a component label to select the Controller or Node Pods. Keep the installation name stable during upgrades. See [Helm built-in objects](https://helm.sh/docs/chart_template_guide/builtin_objects/).

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
# Run the same offline lint/render and CronJob name checks as CI.
bash .github/scripts/check-helm.sh
# Render your settings locally without contacting the cluster.
helm template pve-csi charts/pve-csi --namespace pve-csi --kube-version 1.34.0 -f my-values.yaml
```

For local rendering, set `--kube-version` to the target Kubernetes version; these examples use the minimum supported release. Helm rejects versions below 1.34. The values schema rejects invalid PVE host names and invalid StorageClass, Secret, and CA ConfigMap names before deployment.

`ci/values.yaml` contains fictional inputs for offline checks, not deployment settings. Empty or invalid image tags fail validation. The example defaults can render successfully; rendering does not verify that your image, PVE host, storage, or Secret actually exists.

Install each `driverName` only once per cluster. Do not change the driver name or disk owner VMID while its PVs exist. StorageClass provisioning fields cannot be edited in place: create a differently named class when changing storage pools; existing PVs keep their settings. Multiple Controller replicas, custom kubelet paths, expansion, and snapshots are not supported.

Before uninstalling, handle the consuming Pods and PVCs and confirm unmounting, detachment, and intended disk deletion have completed. Upgrades do not require deleting PVCs.
