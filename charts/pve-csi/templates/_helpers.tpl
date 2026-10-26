{{- define "pve-csi.labels" -}}
app.kubernetes.io/name: pve-csi
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
{{- end }}

{{- define "pve-csi.image" -}}
{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- end }}

{{- /* CronJob names are limited to 52 characters. Preserve existing short names. */ -}}
{{- define "pve-csi.checkName" -}}
{{- $name := printf "%s-check" .Release.Name -}}
{{- if le (len $name) 52 -}}
{{- $name -}}
{{- else -}}
{{- printf "%s-%s-check" (.Release.Name | trunc 37 | trimAll ".-") (.Release.Name | sha256sum | trunc 8) -}}
{{- end -}}
{{- end }}
