{{- define "external-dns-openwrt.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "external-dns-openwrt.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "external-dns-openwrt.selectorLabels" -}}
app.kubernetes.io/name: {{ include "external-dns-openwrt.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "external-dns-openwrt.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "external-dns-openwrt.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "external-dns-openwrt.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "external-dns-openwrt.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "external-dns-openwrt.passwordSecret" -}}
{{- default (include "external-dns-openwrt.fullname" .) .Values.router.existingSecret }}
{{- end }}

{{- /* Fails rendering on values the schema cannot express. */ -}}
{{- define "external-dns-openwrt.validate" -}}
{{- if kindIs "invalid" .Values.ownershipID }}
{{- fail "ownershipID is required: set it to a stable ID for this install, or to \"\" to disable ownership (see the README first)" }}
{{- end }}
{{- $_ := required "router.hostname is required" .Values.router.hostname }}
{{- if and (not .Values.router.password) (not .Values.router.existingSecret) }}
{{- fail "set router.password or router.existingSecret" }}
{{- end }}
{{- if and .Values.router.password .Values.router.existingSecret }}
{{- fail "set router.password or router.existingSecret, not both" }}
{{- end }}
{{- if and .Values.router.caBundle.configMapName .Values.router.caBundle.secretName }}
{{- fail "router.caBundle: set configMapName or secretName, not both" }}
{{- end }}
{{- if and (eq .Values.ownershipID "") (eq .Values.externalDNS.policy "sync") }}
{{- fail "ownershipID \"\" with policy sync would delete every record on the router the cluster does not ask for: set an ownershipID or policy upsert-only" }}
{{- end }}
{{- if not .Values.externalDNS.domainFilters }}
{{- fail "externalDNS.domainFilters is required: without one, anyone who can create an Ingress or a Service can publish any name for the whole LAN" }}
{{- end }}
{{- if and .Values.serviceMonitor.enabled (not .Values.service.enabled) }}
{{- fail "serviceMonitor.enabled needs service.enabled" }}
{{- end }}
{{- end }}
