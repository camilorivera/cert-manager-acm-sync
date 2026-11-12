{{/*
Expand the name of the chart.
*/}}
{{- define "cert-manager-acm-sync.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "cert-manager-acm-sync.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "cert-manager-acm-sync.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "cert-manager-acm-sync.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "cert-manager-acm-sync.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cert-manager-acm-sync.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name
*/}}
{{- define "cert-manager-acm-sync.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cert-manager-acm-sync.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image tag — defaults to appVersion
*/}}
{{- define "cert-manager-acm-sync.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{/*
Validates the dns values and renders --dns-zones as name[=id],...
*/}}
{{- define "cert-manager-acm-sync.dnsZones" -}}
{{- $dns := .Values.dns -}}
{{- if not (has $dns.provider (list "route53" "dnsimple")) -}}
{{- fail (printf "dns.provider must be route53 or dnsimple, got %q" $dns.provider) -}}
{{- end -}}
{{- if or (not $dns.ownerId) (eq (toString $dns.ownerId) "default") -}}
{{- fail "dns.ownerId is required when dns.provider is set and must be unique per deployment, not \"default\"" -}}
{{- end -}}
{{- if or (contains "," (toString $dns.ownerId)) (contains "=" (toString $dns.ownerId)) -}}
{{- fail "dns.ownerId must not contain \",\" or \"=\"" -}}
{{- end -}}
{{- if not $dns.zones -}}
{{- fail "dns.zones must list at least one zone when dns.provider is set" -}}
{{- end -}}
{{- if eq $dns.provider "dnsimple" -}}
{{- if not $dns.dnsimple.accountId -}}
{{- fail "dns.dnsimple.accountId is required when dns.provider is dnsimple" -}}
{{- end -}}
{{- if not $dns.dnsimple.existingSecret.name -}}
{{- fail "dns.dnsimple.existingSecret.name is required when dns.provider is dnsimple" -}}
{{- end -}}
{{- end -}}
{{- $zones := list -}}
{{- range $dns.zones -}}
{{- if eq $dns.provider "route53" -}}
{{- if not .id -}}
{{- fail (printf "dns.zones entry %q needs an id for route53" .name) -}}
{{- end -}}
{{- end -}}
{{- if .id -}}
{{- $zones = append $zones (printf "%s=%s" .name .id) -}}
{{- else -}}
{{- $zones = append $zones .name -}}
{{- end -}}
{{- end -}}
{{- join "," $zones -}}
{{- end }}
