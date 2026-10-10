{{/*
Standard name/label helpers, the same shape `helm create` scaffolds; mirrors booth-streamlit's
own chart helpers, nothing booth-spark-specific here.
*/}}

{{- define "booth-spark.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "booth-spark.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "booth-spark.labels" -}}
app.kubernetes.io/name: {{ include "booth-spark.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
booth.projectbooth.io/module: spark
{{- end -}}

{{- define "booth-spark.selectorLabels" -}}
app.kubernetes.io/name: {{ include "booth-spark.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "booth-spark.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "booth-spark.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The value of the booth.projectbooth.io/spark-run label on this install's run namespaces, which
both admission policies key on: "<release namespace>.<fullname>", at most 63 characters.
*/}}
{{- define "booth-spark.instance" -}}
{{- printf "%s.%s" .Release.Namespace (include "booth-spark.fullname" .) | trunc 63 | trimSuffix "-" | trimSuffix "." -}}
{{- end -}}
