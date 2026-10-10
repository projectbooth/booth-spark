{{/*
Install-time checks (ADR 0110 ruling 2). Chart.yaml's kubeVersion already refuses Kubernetes below
1.30; this also refuses a cluster that doesn't serve ValidatingAdmissionPolicy at all (a
distribution with the API turned off), because every run namespace is fenced by two such policies
(docs/design-v0.md item 3) and a run without them would not be isolated. Included from
boothmodule.yaml, so it runs on every install, upgrade and template.
*/}}
{{- define "booth-spark.checks" -}}
{{- if not (.Capabilities.APIVersions.Has "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy") -}}
{{- fail (printf "booth-spark needs Kubernetes 1.30 or later with the admissionregistration.k8s.io/v1 ValidatingAdmissionPolicy API served (this cluster: %s). Every Spark run's namespace is fenced by admission policies; without them runs would not be isolated." .Capabilities.KubeVersion.Version) -}}
{{- end -}}
{{- if not (has .Values.submit.minRole (list "editor" "owner")) -}}
{{- fail (printf "submit.minRole must be \"editor\" or \"owner\" (viewers never submit, ADR 0110), not %q" .Values.submit.minRole) -}}
{{- end -}}
{{- if and .Values.dataAccess.enabled (contains "apache/spark" .Values.runs.image) -}}
{{- fail "dataAccess.enabled needs runs.image to be booth-spark's runtime image (images/spark-runtime), which adds the JDBC driver, Iceberg, S3A and the module's credential helpers to Spark; plain apache/spark has none of them" -}}
{{- end -}}
{{- if and .Values.dataAccess.enabled (not (contains "@sha256:" .Values.dataAccess.sidecar.image)) -}}
{{- fail "dataAccess.sidecar.image must be pinned by digest" -}}
{{- end -}}
{{- if and .Values.oidc.issuerUrl (not .Values.oidc.clientId) -}}
{{- fail "oidc.clientId is required when oidc.issuerUrl is set" -}}
{{- end -}}
{{- end -}}

{{/* The backend's own image, which is also each data run's agent. */}}
{{- define "booth-spark.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/* The backend's internal port (data access). */}}
{{- define "booth-spark.internalUrl" -}}
{{- printf "http://%s.%s.svc:8081" (include "booth-spark.fullname" .) .Release.Namespace -}}
{{- end -}}

{{/* The Secret holding the database connection string (see values.yaml `postgres`). */}}
{{- define "booth-spark.dsnSecretName" -}}
{{- if .Values.postgres.provisionedByCore -}}
{{- default "booth-database-credentials" .Values.postgres.dsnSecret.name -}}
{{- else -}}
{{- required "postgres.dsnSecret.name is required when postgres.provisionedByCore is false: name the Secret holding your own database's connection string" .Values.postgres.dsnSecret.name -}}
{{- end -}}
{{- end -}}
