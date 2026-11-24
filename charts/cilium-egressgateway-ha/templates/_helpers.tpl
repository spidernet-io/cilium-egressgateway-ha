{{- define "egwha.fullname" -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "egwha.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Image of a component: include "egwha.image" (dict "root" $ "image" .Values.controller.image) */}}
{{- define "egwha.image" -}}
{{- $img := .image -}}
{{- $registry := .root.Values.global.imageRegistryOverride | default $img.registry -}}
{{- if $registry -}}{{ $registry }}/{{- end -}}{{ $img.repository }}
{{- if $img.digest -}}@{{ $img.digest }}{{- else -}}:{{ $img.tag | default .root.Chart.AppVersion }}{{- end -}}
{{- end -}}

{{/* Renders global.egressNodeSelector as node affinity match expressions. */}}
{{- define "egwha.egressNodeMatchExpressions" -}}
{{- $sel := .Values.global.egressNodeSelector -}}
{{- if and (empty $sel.matchLabels) (empty $sel.matchExpressions) -}}
{{- fail "global.egressNodeSelector must not be empty" -}}
{{- end -}}
{{- range $k, $v := $sel.matchLabels }}
- key: {{ $k }}
  operator: In
  values:
  - {{ $v | quote }}
{{- end }}
{{- with $sel.matchExpressions }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* CEL: the policy references the active label in its nodeSelector. */}}
{{- define "egwha.managedPolicyExpression" -}}
has(object.spec.egressGateway) && has(object.spec.egressGateway.nodeSelector) && ((has(object.spec.egressGateway.nodeSelector.matchLabels) && 'egress-gw-active' in object.spec.egressGateway.nodeSelector.matchLabels) || (has(object.spec.egressGateway.nodeSelector.matchExpressions) && object.spec.egressGateway.nodeSelector.matchExpressions.exists(e, e.key == 'egress-gw-active')))
{{- end -}}
