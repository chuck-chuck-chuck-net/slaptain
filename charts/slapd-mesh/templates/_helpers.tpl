{{/*
Common labels.

Deliberately free of .Release.Name and .Release.Namespace. Every object this
chart renders is mesh-scoped and must come out byte-identical at every site
(ADR-028 §3), and a release name is a per-installation fact — one site calling
its release "slapd" and another "ldap-prod" would silently produce two different
manifests and defeat the hash-comparison parity check the whole design rests on.
Object names therefore come from .Values, never from the release.

.Release.Service is the one release value used, and only because Helm always
sets it to the literal "Helm".
*/}}
{{- define "slapd-mesh.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Values.cluster.name }}
app.kubernetes.io/part-of: {{ .Values.mesh.name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Render-time validation.

Everything checked here is decidable from .Values alone, so the verdict is the
same at every site — a check that could pass at one site and fail at its
neighbour would itself be a per-site behaviour and thus the very thing ADR-028
§3 forbids.

Four classes, all of them failures the API server cannot catch for us:

  1. A missing or duplicated site name / serverIDIndex. Two sites sharing one
     decade is the collision ADR-028 §4 names as the first thing to verify:
     slapd validates nothing, contextCSN keeps the highest CSN per serverID, and
     the only symptom is that some writes never propagate.
  2. A site selector naming a site the mesh does not declare — a typo that would
     otherwise shrink the peer set into a half-connected topology that looks
     healthy from both ends of every link that does exist.
  3. A seeded database without seed.site on a multi-site mesh. This is the
     ADR-025 seed race: applied identically to N sites, every site seeds, and
     one pod ends up with a permanent hidden glue suffix — invisible to ordinary
     searches, clean on every CSN health read, and making every backup from that
     pod unrestorable. The failure this chart exists to make unreachable.
  4. Two databases sharing a ridBase. ADR-003 requires one per database; slapd
     silently confuses the stanzas otherwise.

Invoked once, from slapdmesh.yaml, which is always rendered.
*/}}
{{- define "slapd-mesh.validate" -}}
{{- $sites := .Values.mesh.sites -}}
{{- if not $sites -}}
{{- fail "slapd-mesh: mesh.sites is required and must list every site of the mesh. There is deliberately no default: a chart that quietly renders a one-site mesh would hand every site the same serverID decade the moment a second one is added." -}}
{{- end -}}
{{- $names := list -}}
{{- $indices := list -}}
{{- range $i, $s := $sites -}}
  {{- if not $s.name -}}
  {{- fail (printf "slapd-mesh: mesh.sites[%d] has no name" $i) -}}
  {{- end -}}
  {{- /* hasKey is not enough: `serverIDIndex:` with nothing after it parses as
         an explicit null, which hasKey reports as present. kindIs "invalid" is
         how a nil is detected. */ -}}
  {{- if or (not (hasKey $s "serverIDIndex")) (kindIs "invalid" $s.serverIDIndex) -}}
  {{- fail (printf "slapd-mesh: mesh.sites[%d] (%s) has no serverIDIndex. It is required and never inferred from list position — olcServerID is baked into every CSN a pod has written, so a site whose number moves files its future writes under a different sid from its history." $i $s.name) -}}
  {{- end -}}
  {{- if has $s.name $names -}}
  {{- fail (printf "slapd-mesh: site %q is declared twice in mesh.sites" $s.name) -}}
  {{- end -}}
  {{- /* Compared as strings, deliberately. `has` is reflect.DeepEqual under the
         hood, so an int64 from --set and an int from values.yaml are not equal
         to it and the duplicate would slip through — a silent miss on the one
         check whose whole job is catching a silent collision. */ -}}
  {{- $idx := printf "%v" $s.serverIDIndex -}}
  {{- if has $idx $indices -}}
  {{- fail (printf "slapd-mesh: serverIDIndex %s is claimed by two sites (at mesh.sites[%d], %s). Two sites on one decade merge into one contextCSN bucket and each other's writes read as already-seen." $idx $i $s.name) -}}
  {{- end -}}
  {{- $names = append $names $s.name -}}
  {{- $indices = append $indices $idx -}}
{{- end -}}

{{- range $sel := .Values.cluster.sites -}}
  {{- if not (has $sel $names) -}}
  {{- fail (printf "slapd-mesh: cluster.sites names %q, which is not a site in mesh.sites (%s)" $sel (join ", " $names)) -}}
  {{- end -}}
{{- end -}}

{{- $multiSite := gt (len $sites) 1 -}}
{{- $ridBases := list -}}
{{- range $i, $db := .Values.databases -}}
  {{- if not $db.name -}}
  {{- fail (printf "slapd-mesh: databases[%d] has no name" $i) -}}
  {{- end -}}
  {{- $spec := default (dict) $db.spec -}}
  {{- if not $spec.suffix -}}
  {{- fail (printf "slapd-mesh: databases[%d] (%s) has no spec.suffix" $i $db.name) -}}
  {{- end -}}
  {{- $seed := default (dict) $spec.seed -}}
  {{- if and $multiSite $spec.seed (not $seed.site) -}}
  {{- fail (printf "slapd-mesh: database %q declares a seed but no seed.site, and this mesh has %d sites. Applied identically everywhere that seeds at every site — the ADR-025 race that leaves one pod with a permanent hidden glue suffix. Name the founder site: spec.seed.site (one of: %s)." $db.name (len $sites) (join ", " $names)) -}}
  {{- end -}}
  {{- if and $seed.site (not (has $seed.site $names)) -}}
  {{- fail (printf "slapd-mesh: database %q names seed.site %q, which is not a site in mesh.sites (%s). No site would ever match it, so the database would never be seeded anywhere." $db.name $seed.site (join ", " $names)) -}}
  {{- end -}}
  {{- $repl := default (dict) $spec.replication -}}
  {{- if and (hasKey $repl "ridBase") (not (kindIs "invalid" $repl.ridBase)) -}}
    {{- $rid := printf "%v" $repl.ridBase -}}
    {{- if has $rid $ridBases -}}
    {{- fail (printf "slapd-mesh: ridBase %s is used by two databases (at databases[%d], %s). ADR-003 requires a unique ridBase per database within a site." $rid $i $db.name) -}}
    {{- end -}}
    {{- $ridBases = append $ridBases $rid -}}
  {{- end -}}
{{- end -}}

{{- range $i, $sc := .Values.schemas -}}
  {{- if not $sc.name -}}
  {{- fail (printf "slapd-mesh: schemas[%d] has no name" $i) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Render one child CR spec: the user's spec block with clusterRef pinned to this
chart's SlapdCluster.

clusterRef is not a user value. A SlapdDatabase in this bundle belongs to the
SlapdCluster in the same bundle by construction, and letting it be typed would
add a second name to keep in sync for no expressive gain. Any clusterRef the
user does set is dropped rather than merged, so the two can never disagree.

Call with a dict: (dict "spec" $db.spec "clusterName" $name).
*/}}
{{- define "slapd-mesh.childSpec" -}}
{{- $spec := omit (default (dict) .spec) "clusterRef" -}}
{{- toYaml (merge (dict "clusterRef" .clusterName) $spec) -}}
{{- end -}}
