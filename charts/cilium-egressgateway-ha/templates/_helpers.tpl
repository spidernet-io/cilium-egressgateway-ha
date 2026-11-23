{{- define "egwha.fullname" -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "egwha.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "egwha.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{/* Renders egressGatewayHA.candidateNodeSelector as node affinity match expressions. */}}
{{- define "egwha.candidateMatchExpressions" -}}
{{- $sel := .Values.egressGatewayHA.candidateNodeSelector -}}
{{- if and (empty $sel.matchLabels) (empty $sel.matchExpressions) -}}
{{- fail "egressGatewayHA.candidateNodeSelector must not be empty" -}}
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
