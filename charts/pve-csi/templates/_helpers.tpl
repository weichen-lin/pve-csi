{{- define "pve-csi.labels" -}}
app.kubernetes.io/name: pve-csi
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
{{- end }}

{{- define "pve-csi.image" -}}
{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- end }}
