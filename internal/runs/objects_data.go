package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DataCluster is what a run's data access needs from the install (the chart's dataAccess values).
type DataCluster struct {
	// AgentImage is the backend's own image: the agent and the start gate are its binary.
	AgentImage string
	// SidecarImage is booth-core's credential sidecar, digest-pinned.
	SidecarImage string
	// InternalURL is the backend's internal port (http://<backend>.<namespace>.svc:8081): the
	// agents fetch tokens there, the sidecars send their broker calls there, and Iceberg's REST
	// catalog goes there, so no run pod ever has a route to booth-core.
	InternalURL  string
	InternalPort int32
	// Database and ObjectStore are where the sidecars and Spark connect directly: booth-database's
	// Postgres pods, and the object store (an in-cluster MinIO; an empty PodSelector is no rule,
	// e.g. AWS S3, reached through runs.egress.mode=open).
	Database    EgressPeer
	ObjectStore EgressPeer
	// RefreshMax caps the agent's refresh interval (a test knob; empty = two thirds of a token's
	// life).
	RefreshMax string
}

// EgressPeer is one in-cluster destination of a run's pods.
type EgressPeer struct {
	NamespaceSelector map[string]string `json:"namespaceSelector"`
	PodSelector       map[string]string `json:"podSelector"`
	Port              int32             `json:"port"`
}

// Paths and names inside a data run's pods. The token volume is the agent's to write and every
// other container's to read; the bearer is mounted into the agent and the start gate only.
const (
	DataSecret        = "data"
	dataBearerKey     = "bearer"
	dataSecretPath    = "/opt/booth/data"
	tokenDir          = "/var/run/booth/token"
	TokenFile         = tokenDir + "/token"
	s3Root            = "/var/run/booth/s3"
	mainDir           = "/opt/booth/main"
	pgListen          = "127.0.0.1:5432"
	s3HealthBase      = 8091
	LakehouseCatalog  = "lakehouse"
	credentialsSuffix = "/credentials"
)

// Sidecar sizes, per container. They count against the run's quota and its admission footprint.
var (
	sidecarMemMi = 64
	agentMemMi   = 32
)

// SidecarMi is the memory every pod of a run adds for its data access: the agent, a postgres
// sidecar, one s3 sidecar per location, asked for at submission (before anything is resolved).
func (v Validated) SidecarMi() int {
	if !v.NeedsData() {
		return 0
	}
	n := 0
	if d := v.DataAccess; d != nil {
		if d.Database {
			n++
		}
		if d.Lakehouse {
			n++
		}
		n += len(d.Storage)
	}
	return agentMemMi + n*sidecarMemMi
}

// WorkspaceDatabase is the workspace's database name in booth-database (its
// internal/naming.ForWorkspace; booth-streamlit and booth-notebooks compute it the same way).
// Informational: the sidecar connects to whatever its lease names; this makes DATABASE_URL and
// current_database() agree.
func WorkspaceDatabase(workspace string) string {
	sum := sha256.Sum256([]byte("booth-database/workspace/" + workspace))
	return "bdb_ws_" + hex.EncodeToString(sum[:])[:24]
}

func s3Dir(name string) string { return s3Root + "/" + name }

// dataLocations names each s3 lease of a plan: "warehouse", then "storage-<i>".
type dataLocation struct {
	name   string
	scheme string // s3 for the warehouse, s3a for a storage location
	loc    Location
	health int
}

func (p *DataPlan) locations() []dataLocation {
	var out []dataLocation
	if p.Warehouse != nil {
		out = append(out, dataLocation{name: "warehouse", scheme: "s3", loc: *p.Warehouse})
	}
	for i, l := range p.Storage {
		out = append(out, dataLocation{name: "storage-" + strconv.Itoa(i), scheme: "s3a", loc: l})
	}
	for i := range out {
		out[i].health = s3HealthBase + i
	}
	return out
}

// MainFileName is where a run's booth-storage entry point lands in its driver.
func MainFileName(v Validated) string {
	if v.Main.Jar != nil {
		return mainDir + "/app.jar"
	}
	return mainDir + "/main.py"
}

func dataResources(memMi int, cpuLimit string) corev1.ResourceRequirements {
	mem := resource.MustParse(strconv.Itoa(memMi) + "Mi")
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: mem},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuLimit), corev1.ResourceMemory: mem, corev1.ResourceEphemeralStorage: resource.MustParse("16Mi")},
	}
}

// dataPodParts is what data access adds to one pod (the driver, or every executor through the
// template): native sidecars in start order, the start gate, volumes, and the Spark container's
// env and mounts.
type dataPodParts struct {
	sidecars []corev1.Container
	gate     corev1.Container
	volumes  []corev1.Volume
	env      []corev1.EnvVar
	mounts   []corev1.VolumeMount
}

func memEmptyDir(mi int) corev1.VolumeSource {
	return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr(resource.MustParse(strconv.Itoa(mi) + "Mi"))}}
}

// dataPod builds a pod's data access. fetchMain: the driver of a run whose entry point is in
// booth-storage; its start gate copies the file in first.
func dataPod(r Run, dc DataCluster, fetchMain bool) dataPodParts {
	p := r.Data
	always := corev1.ContainerRestartPolicyAlways
	sc := restrictedContainer()
	broker := strings.TrimRight(dc.InternalURL, "/") + "/internal/broker"
	var d dataPodParts

	d.volumes = append(d.volumes,
		corev1.Volume{Name: "token", VolumeSource: memEmptyDir(1)},
		corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: DataSecret, DefaultMode: ptr(int32(0o440)),
		}}},
	)
	agentEnv := []corev1.EnvVar{
		{Name: "BOOTH_AGENT_TOKEN_URL", Value: strings.TrimRight(dc.InternalURL, "/") + "/internal/token"},
		{Name: "BOOTH_AGENT_BEARER_FILE", Value: dataSecretPath + "/" + dataBearerKey},
		{Name: "BOOTH_AGENT_TOKEN_FILE", Value: TokenFile},
	}
	if dc.RefreshMax != "" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "BOOTH_AGENT_REFRESH_MAX", Value: dc.RefreshMax})
	}
	// First: the agent. Its startup probe passes once the token is on disk, so every sidecar
	// after it starts with a token to present.
	d.sidecars = append(d.sidecars, corev1.Container{
		Name: "agent", Image: dc.AgentImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{"agent", "token"}, Env: agentEnv, RestartPolicy: &always,
		Resources: dataResources(agentMemMi, "50m"), SecurityContext: sc,
		StartupProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/booth-spark", "agent", "ready"}}},
			PeriodSeconds: 1, FailureThreshold: 120, TimeoutSeconds: 2,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "token", MountPath: tokenDir},
			{Name: "data", MountPath: dataSecretPath, ReadOnly: true},
		},
	})
	tokenRO := corev1.VolumeMount{Name: "token", MountPath: tokenDir, ReadOnly: true}
	var health []string
	if p.Database != "" {
		scope, _ := json.Marshal(map[string]string{"workspace": r.Workspace})
		d.sidecars = append(d.sidecars, corev1.Container{
			Name: "pg-sidecar", Image: dc.SidecarImage, ImagePullPolicy: corev1.PullIfNotPresent,
			Args: []string{
				"--kind=postgres", "--access=" + p.Access(),
				"--scope=" + string(scope), "--workspace=" + r.Workspace,
				"--token-file=" + TokenFile, "--listen=" + pgListen, "--core-url=" + broker,
			},
			RestartPolicy: &always, Resources: dataResources(sidecarMemMi, "100m"), SecurityContext: sc,
			VolumeMounts: []corev1.VolumeMount{tokenRO},
		})
		health = append(health, "http://"+pgListen+"/healthz")
		url := "postgresql://localhost:5432/" + p.Database + "?sslmode=disable"
		d.env = append(d.env,
			corev1.EnvVar{Name: "DATABASE_URL", Value: url},
			corev1.EnvVar{Name: "JDBC_DATABASE_URL", Value: "jdbc:" + url},
		)
	}
	var storage []map[string]string
	for _, l := range p.locations() {
		dir := s3Dir(l.name)
		vol := "s3-" + l.name
		d.volumes = append(d.volumes, corev1.Volume{Name: vol, VolumeSource: memEmptyDir(1)})
		scope, _ := json.Marshal(map[string]string{"backendId": l.loc.BackendID, "path": l.loc.Path})
		listen := "127.0.0.1:" + strconv.Itoa(l.health)
		d.sidecars = append(d.sidecars, corev1.Container{
			Name: "s3-" + l.name, Image: dc.SidecarImage, ImagePullPolicy: corev1.PullIfNotPresent,
			Args: []string{
				"--kind=s3", "--access=" + l.loc.Access,
				"--scope=" + string(scope), "--workspace=" + r.Workspace,
				"--token-file=" + TokenFile, "--credentials-file=" + dir + credentialsSuffix,
				"--health-listen=" + listen, "--core-url=" + broker,
			},
			RestartPolicy: &always, Resources: dataResources(sidecarMemMi, "100m"), SecurityContext: sc,
			VolumeMounts: []corev1.VolumeMount{tokenRO, {Name: vol, MountPath: dir}},
		})
		health = append(health, "http://"+listen+"/healthz")
		d.mounts = append(d.mounts, corev1.VolumeMount{Name: vol, MountPath: dir, ReadOnly: true})
		if l.name == "warehouse" {
			d.env = append(d.env,
				corev1.EnvVar{Name: "BOOTH_LAKEHOUSE_CATALOG", Value: LakehouseCatalog},
				corev1.EnvVar{Name: "BOOTH_WAREHOUSE_ROOT", Value: l.loc.Root},
			)
		} else {
			storage = append(storage, map[string]string{"backendId": l.loc.BackendID, "path": l.loc.Path, "access": l.loc.Access, "root": l.loc.Root})
		}
	}
	if len(storage) > 0 {
		raw, _ := json.Marshal(storage)
		d.env = append(d.env, corev1.EnvVar{Name: "BOOTH_STORAGE", Value: string(raw)})
	}
	d.env = append(d.env,
		corev1.EnvVar{Name: "BOOTH_WORKSPACE", Value: r.Workspace},
		corev1.EnvVar{Name: "BOOTH_DATA_ROLE", Value: p.Role},
	)
	d.mounts = append(d.mounts, tokenRO)

	// Last: the start gate, a plain init container. It waits for every sidecar's first lease (their
	// health endpoints are loopback-only, so a kubelet probe can't reach them), and in the driver of
	// a run whose entry point is in booth-storage, copies that file in. Spark starts after it.
	gateArgs := []string{"agent", "wait"}
	for _, h := range health {
		gateArgs = append(gateArgs, "--healthz="+h)
	}
	gateMounts := []corev1.VolumeMount{tokenRO}
	if fetchMain {
		gateArgs = append(gateArgs, "--fetch="+strings.TrimRight(dc.InternalURL, "/")+"/internal/main", "--to="+MainFileName(r.Spec))
		gateMounts = append(gateMounts, corev1.VolumeMount{Name: "data", MountPath: dataSecretPath, ReadOnly: true},
			corev1.VolumeMount{Name: "main", MountPath: mainDir})
		d.volumes = append(d.volumes, corev1.Volume{Name: "main", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: ptr(resource.MustParse(strconv.Itoa(MaxMainFileBytes>>20+16) + "Mi")),
		}}})
		d.mounts = append(d.mounts, corev1.VolumeMount{Name: "main", MountPath: mainDir, ReadOnly: true})
	}
	d.gate = corev1.Container{
		Name: "start-gate", Image: dc.AgentImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Args: gateArgs, Env: []corev1.EnvVar{{Name: "BOOTH_AGENT_BEARER_FILE", Value: dataSecretPath + "/" + dataBearerKey}},
		Resources: dataResources(agentMemMi, "100m"), SecurityContext: sc, VolumeMounts: gateMounts,
	}
	return d
}

func (d dataPodParts) apply(spec *corev1.PodSpec, container string) {
	spec.InitContainers = append(spec.InitContainers, d.sidecars...)
	spec.InitContainers = append(spec.InitContainers, d.gate)
	spec.Volumes = append(spec.Volumes, d.volumes...)
	for i := range spec.Containers {
		if spec.Containers[i].Name == container {
			spec.Containers[i].Env = append(spec.Containers[i].Env, d.env...)
			spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, d.mounts...)
		}
	}
}

// DataConf is the Spark configuration of a run's data access: Iceberg's REST catalog through the
// backend's internal port, authenticated by the run's token file on every call; and S3A per bucket,
// each bucket's credentials re-read from its sidecar's file (images/spark-runtime). The warehouse
// is s3:// (the locations booth-lakehouse's catalog gives), a storage location s3a://, so the two
// can share a bucket. Two storage locations can't (Prepare refuses it).
func DataConf(r Run, dc DataCluster) map[string]string {
	p := r.Data
	conf := map[string]string{}
	locs := p.locations()
	if len(locs) == 0 {
		return conf
	}
	const provider = "io.projectbooth.spark.SidecarCredentialsProvider"
	conf["spark.hadoop.fs.s3.impl"] = "org.apache.hadoop.fs.s3a.S3AFileSystem"
	// Any other bucket: no credentials (a clear error from the provider), never the SDK's default
	// chain (environment, instance metadata).
	conf["spark.hadoop.fs.s3a.aws.credentials.provider"] = provider
	for _, l := range locs {
		b := "spark.hadoop.fs.s3a.bucket." + l.loc.Bucket + "."
		conf[b+"aws.credentials.provider"] = provider
		conf[b+"booth.credentials."+l.scheme] = s3Dir(l.name) + credentialsSuffix
		if l.loc.Endpoint != "" {
			conf[b+"endpoint"] = l.loc.Endpoint
		}
		region := l.loc.Region
		if region == "" {
			region = "us-east-1"
		}
		conf[b+"endpoint.region"] = region
		conf[b+"path.style.access"] = strconv.FormatBool(l.loc.PathStyle)
	}
	if p.Warehouse != nil {
		c := "spark.sql.catalog." + LakehouseCatalog
		conf["spark.sql.extensions"] = "org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions"
		conf[c] = "org.apache.iceberg.spark.SparkCatalog"
		conf[c+".type"] = "rest"
		conf[c+".uri"] = strings.TrimRight(dc.InternalURL, "/") + "/internal/lakehouse/iceberg"
		// booth-lakehouse answers /v1/config for the caller's own warehouse whatever is asked.
		conf[c+".warehouse"] = r.Workspace
		conf[c+".header.X-Workspace"] = r.Workspace
		conf[c+".rest.auth.type"] = "io.projectbooth.spark.TokenFileAuthManager"
		conf[c+".booth.token-file"] = TokenFile
		conf[c+".io-impl"] = "org.apache.iceberg.hadoop.HadoopFileIO"
	}
	return conf
}

// dataNetworkPolicies let a data run's pods reach the backend's internal port, booth-database's
// Postgres and the object store, and nothing more of the cluster.
func dataNetworkPolicies(ns string, r Run, dc DataCluster, c Cluster, labels map[string]string) []*networkingv1.NetworkPolicy {
	tcp := ptr(corev1.ProtocolTCP)
	np := func(name string, peer EgressPeer) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: peer.NamespaceSelector},
						PodSelector:       &metav1.LabelSelector{MatchLabels: peer.PodSelector},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: tcp, Port: ptr(intstr.FromInt32(peer.Port))}},
				}},
			},
		}
	}
	out := []*networkingv1.NetworkPolicy{np("to-backend", EgressPeer{
		NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": c.ReleaseNamespace},
		PodSelector:       c.BackendPodLabels, Port: dc.InternalPort,
	})}
	if r.Data.Database != "" && len(dc.Database.PodSelector) > 0 {
		out = append(out, np("to-database", dc.Database))
	}
	if (r.Data.Warehouse != nil || len(r.Data.Storage) > 0) && len(dc.ObjectStore.PodSelector) > 0 {
		out = append(out, np("to-object-store", dc.ObjectStore))
	}
	return out
}

// sameBucket returns the first bucket two storage locations share, if any: S3A takes credentials
// per bucket, so one run can't hold two leases on one bucket under the same scheme.
func sameBucket(locs []Location) string {
	seen := map[string]bool{}
	var names []string
	for _, l := range locs {
		names = append(names, l.Bucket)
	}
	sort.Strings(names)
	for _, b := range names {
		if seen[b] {
			return b
		}
		seen[b] = true
	}
	return ""
}

// CheckPlan refuses a plan this version can't run.
func CheckPlan(p DataPlan) error {
	if b := sameBucket(p.Storage); b != "" {
		return DataRefusal("two of the run's storage locations are in the same bucket (%s); a run may hold one storage location per bucket in this version: name their common parent instead", b)
	}
	for _, l := range append(append([]Location{}, p.Storage...), derefLoc(p.Warehouse)...) {
		if l.Bucket == "" || strings.ContainsAny(l.Bucket, "/ \n") {
			return fmt.Errorf("location %s/%s resolved to no usable bucket", l.BackendID, l.Path)
		}
	}
	return nil
}

func derefLoc(l *Location) []Location {
	if l == nil {
		return nil
	}
	return []Location{*l}
}

// storageRoot is where a location's objects live, as Spark names it.
func storageRoot(scheme, bucket, keyPrefix string) string {
	return scheme + "://" + path.Join(bucket, keyPrefix)
}

// StorageRoot is a declared storage location's s3a:// root.
func StorageRoot(bucket, keyPrefix string) string { return storageRoot("s3a", bucket, keyPrefix) }
