#!/usr/bin/env bash
set -euo pipefail

chart_metadata=$(helm show chart charts/pve-csi)
chart_version=$(printf '%s\n' "$chart_metadata" | awk '/^version:/ { gsub(/["\047]/, "", $2); print $2 }')
app_version=$(printf '%s\n' "$chart_metadata" | awk '/^appVersion:/ { gsub(/["\047]/, "", $2); print $2 }')
image_tag=$(helm show values charts/pve-csi | awk '/^  tag:/ { gsub(/["\047]/, "", $2); print $2 }')
if [[ -z "$chart_version" || "$app_version" != "v${chart_version}" ]]; then
  printf 'Chart.yaml appVersion must equal v followed by the chart version.\n' >&2
  exit 1
fi
if [[ "$image_tag" != "$app_version" ]]; then
  printf 'Default image.tag must match Chart.yaml appVersion.\n' >&2
  exit 1
fi
if [[ "${GITHUB_REF_TYPE:-}" == tag && "${GITHUB_REF_NAME:-}" != "$app_version" ]]; then
  printf 'Release Git tag must match Chart.yaml appVersion and default image.tag.\n' >&2
  exit 1
fi

helm lint charts/pve-csi --strict --kube-version 1.34.0 -f charts/pve-csi/ci/values.yaml
helm template pve-csi charts/pve-csi --kube-version 1.34.0 -f charts/pve-csi/ci/values.yaml > /dev/null
helm template pve-csi charts/pve-csi --kube-version 1.34.0 -f charts/pve-csi/ci/values.yaml \
  --set pve.caConfigMap=example-pve-ca --set storageClass.create=false > /dev/null
helm template pve-csi charts/pve-csi --kube-version 1.34.0 -f charts/pve-csi/ci/values.yaml \
  --set check.enabled=true --set-json 'check.vmids=[100,1000000]' > /dev/null

render_check_name() {
  helm template "$1" charts/pve-csi --kube-version 1.34.0 -f charts/pve-csi/ci/values.yaml \
    --set check.enabled=true --set-json 'check.vmids=[100,1000000]' \
    --show-only templates/check.yaml | awk '/^  name:/ { print $2 }'
}

# Cover name-length boundaries and collisions after truncation.
release_prefix=$(printf '%46s' '' | tr ' ' a)
# A dot at the truncation boundary must not leave a label starting with '-'.
dotted_release="$(printf '%36s' '' | tr ' ' a).bbbbbbbbbb"
releases=(pve-csi "$release_prefix" "${release_prefix}a" "${release_prefix}b" "${release_prefix}aaaaaaa" "$dotted_release")
cronjob_name_pattern='^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$'
check_names=()
for release in "${releases[@]}"; do
  check_name=$(render_check_name "$release")
  if [[ ${#check_name} -gt 52 || ! "$check_name" =~ $cronjob_name_pattern ]]; then
    printf 'Invalid CronJob name for release %s: %s\n' "$release" "$check_name" >&2
    exit 1
  fi
  if [[ ${#release} -le 46 && "$check_name" != "$release-check" ]]; then
    printf 'Existing short CronJob name changed for release %s: %s\n' "$release" "$check_name" >&2
    exit 1
  fi
  if [[ ${#check_names[@]} -gt 0 ]]; then
    for previous_name in "${check_names[@]}"; do
      if [[ "$check_name" == "$previous_name" ]]; then
        printf 'CronJob name collision for release %s: %s\n' "$release" "$check_name" >&2
        exit 1
      fi
    done
  fi
  check_names+=("$check_name")
done

package_dir=$(mktemp -d)
trap 'rm -rf "$package_dir"' EXIT
helm package charts/pve-csi --destination "$package_dir"
helm template pve-csi charts/pve-csi --kube-version 1.34.0 > "$package_dir/source.yaml"
helm template pve-csi "$package_dir/pve-csi-${chart_version}.tgz" --kube-version 1.34.0 > "$package_dir/packaged.yaml"
cmp "$package_dir/source.yaml" "$package_dir/packaged.yaml"
printf 'Helm version, render, package and CronJob name checks passed.\n'
