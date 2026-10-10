package runs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func dataCluster() Cluster {
	c := testCluster()
	c.Data = &DataCluster{
		AgentImage: "ghcr.io/projectbooth/booth-spark:t", SidecarImage: "sidecar@sha256:def",
		InternalURL: "http://booth-spark.booth-spark.svc:8081", InternalPort: 8081,
		Database:    EgressPeer{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "booth-database"}, PodSelector: map[string]string{"app": "pg"}, Port: 5432},
		ObjectStore: EgressPeer{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "minio"}, PodSelector: map[string]string{"app": "minio"}, Port: 9000},
	}
	return c
}

func dataTestRun(t *testing.T, main Main) Run {
	t.Helper()
	v, err := Validate(Spec{Name: "etl", Main: main, DataAccess: &DataAccess{Database: true, Lakehouse: true,
		Storage: []StorageLocation{{BackendID: "lake", Path: "raw"}}}}, dataLimits())
	if err != nil {
		t.Fatal(err)
	}
	return Run{ID: "rdat", Workspace: "acme", Namespace: "bspark-rdat", Spec: v, DataBearer: strings.Repeat("b", 64),
		Data: &DataPlan{Role: "editor", Database: WorkspaceDatabase("acme"),
			Warehouse: &Location{BackendID: "lake", Path: "warehouse", Access: "readwrite", Bucket: "lake", KeyPrefix: "wh", Endpoint: "http://minio:9000", PathStyle: true, Root: "s3://lake/wh"},
			Storage:   []Location{{BackendID: "lake", Path: "raw", Access: "read", Bucket: "lake", KeyPrefix: "raw", Endpoint: "http://minio:9000", PathStyle: true, Root: "s3a://lake/raw"}},
		}}
}

func names(cs []corev1.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func mountedIn(spec corev1.PodSpec, volume string) []string {
	var out []string
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		for _, m := range c.VolumeMounts {
			if m.Name == volume {
				rw := ""
				if !m.ReadOnly {
					rw = "(rw)"
				}
				out = append(out, c.Name+rw)
			}
		}
	}
	return out
}

// Data access in a run's pods (docs/design-v0.md item 4): the agent first, then the sidecars, then
// the start gate, then Spark; the bearer reaches the agent and the gate only; only the agent writes
// the token; the executors get the same through Spark's pod template.
func TestBuild_DataPods(t *testing.T) {
	r := dataTestRun(t, Main{Python: &FileRef{BackendID: "lake", Path: "jobs/etl.py"}})
	o, err := Build(r, dataCluster(), []APIEndpoint{{IP: "172.19.0.3", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	if o.DataSecret == nil || o.DataSecret.StringData[dataBearerKey] != r.DataBearer {
		t.Fatalf("data secret = %+v", o.DataSecret)
	}
	if o.Namespace.Labels[DatabaseClientLabel] != "true" {
		t.Errorf("a run with database access lacks booth-database's client label: %v", o.Namespace.Labels)
	}
	if plain := build(t, testCluster()); plain.Namespace.Labels[DatabaseClientLabel] != "" {
		t.Error("a run without database access has booth-database's client label")
	}
	d := o.DriverPod.Spec
	if got := strings.Join(names(d.InitContainers), ","); got != "agent,pg-sidecar,s3-warehouse,s3-storage-0,start-gate" {
		t.Errorf("driver init containers = %s", got)
	}
	for _, c := range d.InitContainers {
		native := c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		if native == (c.Name == "start-gate") {
			t.Errorf("%s: native sidecar = %t", c.Name, native)
		}
		if sc := c.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 {
			t.Errorf("%s isn't restricted: %+v", c.Name, sc)
		}
		if c.Resources.Limits.Memory().IsZero() {
			t.Errorf("%s has no memory limit", c.Name)
		}
	}
	if d.InitContainers[0].StartupProbe == nil || d.InitContainers[0].StartupProbe.Exec == nil {
		t.Error("the agent has no startup probe: the sidecars would start before the token exists")
	}
	if got := strings.Join(mountedIn(d, "data"), ","); got != "agent,start-gate" {
		t.Errorf("the data bearer is mounted in %s, want the agent and the start gate only", got)
	}
	if got := strings.Join(mountedIn(d, "token"), ","); got != "agent(rw),pg-sidecar,s3-warehouse,s3-storage-0,start-gate,driver" {
		t.Errorf("the token volume is mounted in %s", got)
	}
	gate := strings.Join(d.InitContainers[4].Args, " ")
	for _, want := range []string{"agent wait", "--healthz=http://127.0.0.1:5432/healthz", "--healthz=http://127.0.0.1:8091/healthz", "--healthz=http://127.0.0.1:8092/healthz",
		"--fetch=http://booth-spark.booth-spark.svc:8081/internal/main", "--to=/opt/booth/main/main.py"} {
		if !strings.Contains(gate, want) {
			t.Errorf("start gate %q lacks %q", gate, want)
		}
	}
	env := map[string]string{}
	for _, e := range d.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["DATABASE_URL"] != "postgresql://localhost:5432/"+WorkspaceDatabase("acme")+"?sslmode=disable" || !strings.HasPrefix(env["JDBC_DATABASE_URL"], "jdbc:postgresql://localhost:5432/") ||
		env["BOOTH_WAREHOUSE_ROOT"] != "s3://lake/wh" || env["BOOTH_LAKEHOUSE_CATALOG"] != "lakehouse" || !strings.Contains(env["BOOTH_STORAGE"], `"root":"s3a://lake/raw"`) ||
		env["BOOTH_DATA_ROLE"] != "editor" {
		t.Errorf("driver env = %v", env)
	}
	pg := strings.Join(d.InitContainers[1].Args, " ")
	if !strings.Contains(pg, "--access=readwrite") || !strings.Contains(pg, `--scope={"workspace":"acme"}`) || !strings.Contains(pg, "--core-url=http://booth-spark.booth-spark.svc:8081/internal/broker") {
		t.Errorf("pg sidecar args = %s", pg)
	}
	if !strings.Contains(strings.Join(d.InitContainers[3].Args, " "), "--access=read ") {
		t.Errorf("a read location's sidecar asks for more: %v", d.InitContainers[3].Args)
	}

	// Spark: the entry point from the gate, Iceberg's catalog through the internal port, S3A per
	// bucket with the scheme telling the warehouse from the storage location on one bucket.
	args := strings.Join(d.Containers[0].Args, " ")
	for _, want := range []string{
		"spark.sql.catalog.lakehouse=org.apache.iceberg.spark.SparkCatalog",
		"spark.sql.catalog.lakehouse.uri=http://booth-spark.booth-spark.svc:8081/internal/lakehouse/iceberg",
		"spark.sql.catalog.lakehouse.rest.auth.type=io.projectbooth.spark.TokenFileAuthManager",
		"spark.sql.catalog.lakehouse.header.X-Workspace=acme",
		"spark.hadoop.fs.s3a.bucket.lake.booth.credentials.s3=/var/run/booth/s3/warehouse/credentials",
		"spark.hadoop.fs.s3a.bucket.lake.booth.credentials.s3a=/var/run/booth/s3/storage-0/credentials",
		"spark.hadoop.fs.s3a.bucket.lake.endpoint=http://minio:9000",
		"spark.hadoop.fs.s3a.bucket.lake.path.style.access=true",
		"spark.hadoop.fs.s3a.aws.credentials.provider=io.projectbooth.spark.SidecarCredentialsProvider",
		"spark.hadoop.fs.s3.impl=org.apache.hadoop.fs.s3a.S3AFileSystem",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("driver args lack %s", want)
		}
	}
	if !strings.HasSuffix(args, " /opt/booth/main/main.py") {
		t.Errorf("the entry point isn't the fetched file: ...%s", args[len(args)-60:])
	}

	// The executors, through the template: the same sidecars, no entry-point fetch.
	var tmpl corev1.Pod
	if err := yaml.Unmarshal([]byte(o.AppConfigMap.Data[executorTemplateKey]), &tmpl); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(tmpl.Spec.InitContainers), ","); got != "agent,pg-sidecar,s3-warehouse,s3-storage-0,start-gate" {
		t.Errorf("executor init containers = %s", got)
	}
	if strings.Contains(strings.Join(tmpl.Spec.InitContainers[4].Args, " "), "--fetch") {
		t.Error("an executor fetches the entry point")
	}
	if got := strings.Join(mountedIn(tmpl.Spec, "data"), ","); got != "agent" {
		t.Errorf("executor: the data bearer is mounted in %s", got)
	}

	// Network: the backend's internal port, the database and the object store, nothing else new.
	nps := map[string]bool{}
	for _, p := range o.NetworkPolicies {
		nps[p.Name] = true
		if p.Name == "to-backend" && p.Spec.Egress[0].Ports[0].Port.IntVal != 8081 {
			t.Errorf("to-backend = %+v", p.Spec)
		}
	}
	for _, n := range []string{"to-backend", "to-database", "to-object-store"} {
		if !nps[n] {
			t.Errorf("no %s policy", n)
		}
	}

	// The quota and the admission footprint count the sidecars: agent 32Mi + 3 x 64Mi per pod.
	if r.Spec.SidecarMi() != 224 {
		t.Errorf("SidecarMi = %d", r.Spec.SidecarMi())
	}
	if q := o.Quota.Spec.Hard.Name("limits.memory", ""); q.String() != "3872Mi" {
		t.Errorf("quota memory = %s, want 3 x (896+224) + 512", q)
	}
	if mem := d.Containers[0].Resources.Limits.Memory().String(); mem != "896Mi" {
		t.Errorf("the Spark container's own memory = %s", mem)
	}
}

func TestBuild_DataJar(t *testing.T) {
	r := dataTestRun(t, Main{Jar: &FileRef{BackendID: "lake", Path: "jobs/etl.jar"}, MainClass: "com.example.Etl"})
	o, err := Build(r, dataCluster(), []APIEndpoint{{IP: "172.19.0.3", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(o.DriverPod.Spec.Containers[0].Args, " "); !strings.HasSuffix(args, " --class com.example.Etl /opt/booth/main/app.jar") {
		t.Errorf("jar args end ...%s", args[len(args)-80:])
	}
	// No data access on this install: a data run doesn't build.
	if _, err := Build(r, testCluster(), []APIEndpoint{{IP: "1.2.3.4", Port: 6443}}); err == nil {
		t.Error("built a data run on an install without data access")
	}
}

func TestCheckPlan(t *testing.T) {
	two := DataPlan{Storage: []Location{{Bucket: "a"}, {Bucket: "a"}}}
	if err := CheckPlan(two); !errors.Is(err, ErrDataRefused) {
		t.Errorf("two storage locations on one bucket: %v", err)
	}
	ok := DataPlan{Warehouse: &Location{Bucket: "a"}, Storage: []Location{{Bucket: "a"}, {Bucket: "b"}}}
	if err := CheckPlan(ok); err != nil {
		t.Errorf("the warehouse and a storage location on one bucket: %v", err)
	}
}

// fakeData is runs.Data for the controller tests.
type fakeData struct {
	prepareErr, checkErr error
	prepared, forgot     int
}

func (f *fakeData) Prepare(context.Context, Run) (DataPlan, error) {
	f.prepared++
	if f.prepareErr != nil {
		return DataPlan{}, f.prepareErr
	}
	return DataPlan{Role: "editor", Database: "db"}, nil
}
func (f *fakeData) Check(context.Context, Run) error { return f.checkErr }
func (f *fakeData) Forget(string)                    { f.forgot++ }

func TestController_Data(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	newDataRun := func() Run {
		r := newRun(t, "acme", "u1")
		r.Spec.DataAccess = &DataAccess{Database: true}
		r.DataBearer = NewID() + strings.Repeat("x", 40)
		created, err := s.Create(ctx, r, "", roomy)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	f := &fakeCluster{driver: map[string]DriverState{}}
	data := &fakeData{}
	c := &Controller{Store: s, Cluster: f, Data: data, PendingTimeout: 10 * time.Minute, LogTailBytes: 1 << 20, Now: func() time.Time { return now }}

	// Resolved before the launch, and recorded.
	ok := newDataRun()
	c.Pass(ctx)
	got, _ := s.Get(ctx, ok.ID)
	if !got.Launched || got.Data == nil || got.Data.Role != "editor" || data.prepared == 0 {
		t.Fatalf("not resolved and launched: %+v", got)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), got.DataBearer) {
		t.Error("the data bearer shows in the run's JSON")
	}
	if r, err := s.ByDataBearer(ctx, got.DataBearer); err != nil || r.ID != ok.ID {
		t.Errorf("ByDataBearer: %v", err)
	}

	// A refused submitter while it lives: the run fails and its namespace goes.
	f.driver[ok.Namespace] = DriverState{Exists: true, Phase: corev1.PodRunning}
	data.checkErr = DataRefusal("the run's submitter no longer has access")
	c.Pass(ctx)
	got, _ = s.Get(ctx, ok.ID)
	if got.State != Failed || !strings.Contains(got.Reason, "lost its data access") || data.forgot == 0 {
		t.Errorf("after a refused re-mint: %s %q", got.State, got.Reason)
	}
	if len(f.deleted) == 0 || f.deleted[len(f.deleted)-1] != ok.Namespace {
		t.Errorf("namespace not deleted: %v", f.deleted)
	}
	if _, err := s.ByDataBearer(ctx, got.DataBearer); !errors.Is(err, ErrNotFound) {
		t.Errorf("an ended run's bearer still finds it: %v", err)
	}
	data.checkErr = nil

	// Refused before the launch: failed, never launched.
	refused := newDataRun()
	data.prepareErr = DataRefusal("the workspace has no lakehouse warehouse")
	c.Pass(ctx)
	got, _ = s.Get(ctx, refused.ID)
	if got.State != Failed || got.Launched || !strings.Contains(got.Reason, "no lakehouse warehouse") {
		t.Errorf("refused at launch: %s launched=%t %q", got.State, got.Launched, got.Reason)
	}

	// An outage: retried, the run stays pending until the pending timeout.
	later := newDataRun()
	data.prepareErr = errors.New("booth-core unreachable")
	c.Pass(ctx)
	if got, _ = s.Get(ctx, later.ID); got.State != Pending || got.Launched {
		t.Errorf("after an outage: %s launched=%t", got.State, got.Launched)
	}
	now = now.Add(11 * time.Minute)
	c.Pass(ctx)
	if got, _ = s.Get(ctx, later.ID); got.State != Failed || !strings.Contains(got.Reason, "unreachable") {
		t.Errorf("after the pending timeout: %s %q", got.State, got.Reason)
	}
}
