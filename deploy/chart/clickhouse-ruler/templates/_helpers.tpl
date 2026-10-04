{{/*
Paths are fixed rather than configurable. The rules path in particular has to
agree with what git-sync's --link produces, and a value that can disagree with
the sidecar's flags is a value that can point the ruler at an empty directory.
*/}}

{{- define "clickhouse-ruler.configDir" -}}
/etc/clickhouse-ruler
{{- end -}}

{{/* git-sync's working directory. The synced tree is not this, it is the link below. */}}
{{- define "clickhouse-ruler.gitSyncRoot" -}}
/rules
{{- end -}}

{{/*
The symlink git-sync flips after each successful sync. It points at
.worktrees/<hash> under the root, so this path, not the root, is the tree to
read: the root also holds git's own state.
*/}}
{{- define "clickhouse-ruler.gitSyncLink" -}}
{{ include "clickhouse-ruler.gitSyncRoot" . }}/current
{{- end -}}

{{- define "clickhouse-ruler.reloadScript" -}}
/etc/git-sync/reload.sh
{{- end -}}

{{/* The rules directory handed to `ruler run --rules`. */}}
{{- define "clickhouse-ruler.rulesPath" -}}
{{- $base := "" -}}
{{- if eq .Values.rules.delivery "git-sync" -}}
{{- $base = include "clickhouse-ruler.gitSyncLink" . -}}
{{- else -}}
{{- $base = printf "%s/rules" (include "clickhouse-ruler.configDir" .) -}}
{{- end -}}
{{- with .Values.rules.subdirectory -}}
{{- $base = printf "%s/%s" $base (trimAll "/" .) -}}
{{- end -}}
{{- $base -}}
{{- end -}}

{{- define "clickhouse-ruler.sourcesPath" -}}
{{ include "clickhouse-ruler.configDir" . }}/sources/ruler.yaml
{{- end -}}

{{- define "clickhouse-ruler.policyPath" -}}
{{ include "clickhouse-ruler.configDir" . }}/policy/policy.yaml
{{- end -}}

{{/*
The image to run. A digest wins over the tag, because pinning by digest and
then resolving a tag beside it would run bytes neither value names.
*/}}
{{- define "clickhouse-ruler.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{/*
Directory a named password Secret is mounted at. One directory per Secret, so
several sources sharing one Secret share one mount.
*/}}
{{- define "clickhouse-ruler.passwordSecretDir" -}}
{{ include "clickhouse-ruler.configDir" .ctx }}/secrets/{{ .name }}
{{- end -}}

{{/*
Directory the Alertmanager credential Secret is mounted at. One Secret, because
one set of Alertmanagers shares one credential (spec 6.5).
*/}}
{{- define "clickhouse-ruler.alertmanagerSecretDir" -}}
{{ include "clickhouse-ruler.configDir" . }}/secrets/alertmanager
{{- end -}}

{{/*
Directory the Alertmanager TLS Secret is mounted at. Its own mount rather than
the credential's, because a cluster behind a private CA need not be behind auth
and the two are separate Secrets with separate rotation (spec 6.5).
*/}}
{{- define "clickhouse-ruler.alertmanagerTLSDir" -}}
{{ include "clickhouse-ruler.configDir" . }}/secrets/alertmanager-tls
{{- end -}}

{{/*
Names of every Secret a templated source reads its password from, sorted and
deduplicated, as a JSON list.
*/}}
{{- define "clickhouse-ruler.passwordSecretNames" -}}
{{- $names := list -}}
{{- range .Values.sources -}}
{{- with .passwordSecret -}}
{{- with .name -}}
{{- $names = append $names . -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $names | uniq | sortAlpha | toJson -}}
{{- end -}}

{{- define "clickhouse-ruler.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "clickhouse-ruler.fullname" -}}
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

{{- define "clickhouse-ruler.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "clickhouse-ruler.labels" -}}
helm.sh/chart: {{ include "clickhouse-ruler.chart" . }}
{{ include "clickhouse-ruler.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "clickhouse-ruler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "clickhouse-ruler.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "clickhouse-ruler.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "clickhouse-ruler.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The one refusal the JSON schema cannot state, because both fields have defaults
and neither is wrong on its own. A password in a value is caught by the schema,
which allows no source key the operator's file does not have.
*/}}
{{- define "clickhouse-ruler.validate" -}}
{{- if and .Values.sources .Values.sourcesSecret.name -}}
{{- fail "set either sources or sourcesSecret.name, not both: one templates the operator's file and the other supplies it whole" -}}
{{- end -}}
{{- if and (not .Values.sources) (not .Values.sourcesSecret.name) -}}
{{- fail "no sources: set sources, or sourcesSecret.name to a Secret holding the whole operator's file" -}}
{{- end -}}
{{- if and .Values.sources (not .Values.alertmanagerURLs) -}}
{{- fail "alertmanagerURLs is required, one entry per member of the Alertmanager cluster" -}}
{{- end -}}
{{- if and .Values.sourcesSecret.name .Values.alertmanagerURLs -}}
{{- fail "alertmanagerURLs cannot be set beside sourcesSecret.name: that Secret is the whole operator's file, so its alertmanagers block comes from the Secret too" -}}
{{- end -}}
{{- if and .Values.alertmanagerAuth .Values.alertmanagerAuth.basicAuth .Values.alertmanagerAuth.authorization -}}
{{- fail "set either alertmanagerAuth.basicAuth or alertmanagerAuth.authorization, not both: they write the same Authorization header" -}}
{{- end -}}
{{- $tls := .Values.alertmanagerTLS -}}
{{- if and $tls $tls.secretName -}}
{{- if and .Values.sourcesSecret.name -}}
{{- fail "alertmanagerTLS cannot be set beside sourcesSecret.name: that Secret is the whole operator's file, so its tls_config comes from the Secret too" -}}
{{- end -}}
{{/*
Refused here rather than left to the ruler. The ruler does refuse it, by
exiting 2 at startup, which in a cluster is a pod that will not come up: a
rendered manifest that cannot run is a failed install either way, and this one
names the value.
*/}}
{{- range .Values.alertmanagerURLs -}}
{{- if not (hasPrefix "https://" .) -}}
{{- fail (printf "alertmanagerTLS is set, so every alertmanagerURLs entry has to be https://, and %q is not" .) -}}
{{- end -}}
{{- end -}}
{{/*
A certificate with no key cannot be presented and a key with no certificate is
never sent, which reads as an Alertmanager refusing a credential it was never
offered.
*/}}
{{- if or (and $tls.certKey (not $tls.keyKey)) (and $tls.keyKey (not $tls.certKey)) -}}
{{- fail "set both alertmanagerTLS.certKey and alertmanagerTLS.keyKey, or neither: a client certificate needs its key" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.alertmanagerTLS (not .Values.alertmanagerTLS.secretName) (or .Values.alertmanagerTLS.caKey .Values.alertmanagerTLS.certKey) -}}
{{- fail "alertmanagerTLS.secretName is required: the chart mounts that Secret, and the keys name what is inside it" -}}
{{- end -}}
{{- end -}}

{{/*
A budget that allows no eviction at all blocks node maintenance rather than
shaping it, and the drain it blocks is reported by whatever is draining rather
than here. Percentages are left to the API server, which understands them.
*/}}
{{- define "clickhouse-ruler.validateDisruptionBudget" -}}
{{- $pdb := .Values.podDisruptionBudget -}}
{{- if and $pdb.minAvailable $pdb.maxUnavailable -}}
{{- fail "set either podDisruptionBudget.minAvailable or podDisruptionBudget.maxUnavailable, not both" -}}
{{- end -}}
{{- if and (not $pdb.minAvailable) (not $pdb.maxUnavailable) -}}
{{- fail "podDisruptionBudget.enabled needs one of minAvailable or maxUnavailable" -}}
{{- end -}}
{{- if and $pdb.minAvailable (not (kindIs "string" $pdb.minAvailable)) -}}
{{- if ge (float64 $pdb.minAvailable) (float64 .Values.replicaCount) -}}
{{- fail "podDisruptionBudget.minAvailable is at least replicaCount, which blocks every eviction rather than shaping it" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
How long the pod gets to stop. The ruler drains in-flight evaluations for
--shutdown-timeout and then gives the HTTP surface five more seconds, so a
grace period equal to the timeout has kubelet sending SIGKILL exactly as the
drain ends: the evaluation is cut off anyway and the warning that says so may
never be written. Ten seconds covers the HTTP shutdown and leaves slack.
*/}}
{{- define "clickhouse-ruler.terminationGracePeriodSeconds" -}}
{{- add .Values.ruler.shutdownTimeoutSeconds 10 -}}
{{- end -}}

{{/*
git-sync's flags, shared by the init container that populates the volume and the
sidecar that keeps it current. The exec hook is not here: it belongs only to the
sidecar, because at init time there is no ruler listening to reload.
*/}}
{{- define "clickhouse-ruler.gitSyncArgs" -}}
- --repo={{ required "rules.gitSync.repo is required when rules.delivery is git-sync" .Values.rules.gitSync.repo }}
- --ref={{ .Values.rules.gitSync.ref }}
- --depth={{ .Values.rules.gitSync.depth }}
- --root={{ include "clickhouse-ruler.gitSyncRoot" . }}
- --link={{ include "clickhouse-ruler.gitSyncLink" . }}
{{- with .Values.rules.gitSync.extraArgs }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "clickhouse-ruler.gitSyncVolumeMounts" -}}
- name: rules
  mountPath: {{ include "clickhouse-ruler.gitSyncRoot" . }}
{{- with .Values.rules.gitSync.credentialsSecret }}
- name: git-credentials
  mountPath: /etc/git-secret
  readOnly: true
{{- end }}
{{- end -}}
