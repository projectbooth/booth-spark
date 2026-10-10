package runs

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/sparkconf"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func testCluster() Cluster {
	c := Cluster{
		Instance: "booth-spark.booth-spark", ReleaseNamespace: "booth-spark", BackendServiceAccount: "booth-spark",
		BackendPodLabels:  map[string]string{"app.kubernetes.io/name": "booth-spark", "app.kubernetes.io/instance": "booth-spark"},
		DriverClusterRole: "booth-spark-driver", DriverClusterRoleUID: "uid-1", RunControllerClusterRole: "booth-spark-run-controller",
		Image: "spark@sha256:abc", ImagePullPolicy: corev1.PullIfNotPresent,
		Driver:    Placement{NodeSelector: map[string]string{"pool": "compute"}},
		Executor:  Placement{NodeSelector: map[string]string{"pool": "compute"}, Tolerations: []corev1.Toleration{{Key: "pool", Value: "compute", Effect: corev1.TaintEffectNoSchedule}}},
		DriverCPU: CPU{Request: "250m", Limit: "1"}, ExecutorCPU: CPU{Request: "250m", Limit: "1"},
	}
	c.Egress.Mode = "open"
	c.Egress.ExceptCIDRs = []string{"10.0.0.0/8", "192.168.0.0/16"}
	c.Egress.DNS.NamespaceSelector = map[string]string{"kubernetes.io/metadata.name": "kube-system"}
	c.Egress.DNS.PodSelector = map[string]string{"k8s-app": "kube-dns"}
	return c
}

func testRun(t *testing.T) Run {
	t.Helper()
	v, err := Validate(Spec{Name: "pi", Main: Main{InlinePython: "print(1)"}, Args: []string{"10"}, Conf: map[string]string{"spark.sql.shuffle.partitions": "4"}}, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	return Run{ID: "rabc", Workspace: "acme", Namespace: "bspark-rabc", Spec: v}
}

func build(t *testing.T, c Cluster) Objects {
	t.Helper()
	o, err := Build(testRun(t), c, []APIEndpoint{{IP: "172.19.0.3", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestBuild_Namespace(t *testing.T) {
	ns := build(t, testCluster()).Namespace
	want := map[string]string{
		LabelInstance: "booth-spark.booth-spark", LabelWorkspace: "acme", LabelRun: "rabc", LabelModule: "spark",
		"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest",
	}
	if ns.Name != "bspark-rabc" || !reflect.DeepEqual(ns.Labels, want) {
		t.Errorf("namespace %s labels %v", ns.Name, ns.Labels)
	}
	// Owned by the driver ClusterRole, so uninstalling the chart deletes every run namespace.
	if len(ns.OwnerReferences) != 1 {
		t.Fatalf("owners = %v", ns.OwnerReferences)
	}
	o := ns.OwnerReferences[0]
	if o.Kind != "ClusterRole" || o.Name != "booth-spark-driver" || o.UID != "uid-1" || o.BlockOwnerDeletion != nil {
		t.Errorf("owner = %+v", o)
	}
}

// ADR 0110 step 3, requirement 5: the executor account has no permissions (nothing binds it) and
// mounts no token, and every executor uses it.
func TestBuild_ExecutorAccountHasNothing(t *testing.T) {
	o := build(t, testCluster())
	if o.ExecutorAccount.Name != "executor" || o.ExecutorAccount.AutomountServiceAccountToken == nil || *o.ExecutorAccount.AutomountServiceAccountToken {
		t.Errorf("executor account = %+v", o.ExecutorAccount)
	}
	for _, b := range []string{o.ControllerBinding.Subjects[0].Name, o.DriverBinding.Subjects[0].Name} {
		if b == "executor" {
			t.Error("the executor account is bound to a role")
		}
	}
	var tmpl corev1.Pod
	if err := yaml.Unmarshal([]byte(o.AppConfigMap.Data[executorTemplateKey]), &tmpl); err != nil {
		t.Fatal(err)
	}
	if tmpl.Spec.ServiceAccountName != "executor" || tmpl.Spec.AutomountServiceAccountToken == nil || *tmpl.Spec.AutomountServiceAccountToken {
		t.Errorf("executor template account: %q automount %v", tmpl.Spec.ServiceAccountName, tmpl.Spec.AutomountServiceAccountToken)
	}
	// Spark mounts its own volume at spark.local.dir on executors: the template must not mount
	// anything there (Integration found "/tmp: must be unique").
	for _, m := range tmpl.Spec.Containers[0].VolumeMounts {
		if m.MountPath == localDir {
			t.Errorf("the executor template mounts %s, where Spark adds its own volume", localDir)
		}
	}
	if tmpl.Labels[LabelWorkspace] != "acme" || tmpl.Labels[LabelComponent] != "executor" || tmpl.Spec.NodeSelector["pool"] != "compute" || len(tmpl.Spec.Tolerations) != 1 {
		t.Errorf("executor template labels/placement: %v %v %v", tmpl.Labels, tmpl.Spec.NodeSelector, tmpl.Spec.Tolerations)
	}
	sc := tmpl.Spec.Containers[0].SecurityContext
	if tmpl.Spec.SecurityContext.RunAsNonRoot == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || sc.Capabilities.Drop[0] != "ALL" {
		t.Error("executor template is not hardened")
	}
}

func TestBuild_Bindings(t *testing.T) {
	o := build(t, testCluster())
	cb := o.ControllerBinding
	if cb.RoleRef.Kind != "ClusterRole" || cb.RoleRef.Name != "booth-spark-run-controller" || len(cb.Subjects) != 1 ||
		cb.Subjects[0].Name != "booth-spark" || cb.Subjects[0].Namespace != "booth-spark" {
		t.Errorf("controller binding = %+v", cb)
	}
	db := o.DriverBinding
	if db.RoleRef.Name != "booth-spark-driver" || len(db.Subjects) != 1 || db.Subjects[0].Name != "driver" || db.Subjects[0].Namespace != "bspark-rabc" {
		t.Errorf("driver binding = %+v", db)
	}
}

func TestBuild_DriverPod(t *testing.T) {
	p := build(t, testCluster()).DriverPod
	if p.Spec.ServiceAccountName != "driver" || p.Labels[LabelWorkspace] != "acme" || p.Labels[LabelComponent] != "driver" ||
		p.Spec.NodeSelector["pool"] != "compute" || p.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("driver pod = %+v", p.Spec)
	}
	c := p.Spec.Containers[0]
	if c.Image != "spark@sha256:abc" || c.Resources.Limits.Memory().String() != "896Mi" || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("driver container: %s %s", c.Image, c.Resources.Limits.Memory())
	}
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--master k8s://https://172.19.0.3:6443", "--deploy-mode client",
		"spark.kubernetes.namespace=bspark-rabc", "spark.kubernetes.authenticate.executor.serviceAccountName=executor",
		"spark.kubernetes.container.image=spark@sha256:abc", "spark.authenticate=true",
		"spark.dynamicAllocation.maxExecutors=2", "spark.sql.shuffle.partitions=4",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("driver args lack %q:\n%s", want, args)
		}
	}
	// Every UI setting of internal/sparkconf (step 2's proof used the same).
	for k, v := range sparkconf.UIConf() {
		if !strings.Contains(args, k+"="+v) {
			t.Errorf("driver args lack %s=%s", k, v)
		}
	}
	if !strings.HasSuffix(args, "/opt/booth/run/main.py 10") {
		t.Errorf("the application and its args come last: %s", args)
	}
	if !strings.Contains(args, "spark.local.dir="+localDir) {
		t.Error("spark.local.dir is not the dedicated path")
	}
	mounted := false
	for _, m := range c.VolumeMounts {
		mounted = mounted || m.MountPath == localDir
	}
	if !mounted {
		t.Error("the driver has no writable local dir")
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["APPLICATION_WEB_PROXY_BASE"] != "/iframe/spark/runs/rabc/ui" {
		t.Errorf("env = %v", env)
	}
}

// Driver only: Spark refuses dynamic allocation with maxExecutors=0, so it is turned off.
func TestDriverArgs_NoExecutors(t *testing.T) {
	r := testRun(t)
	r.Spec.MinExecutors, r.Spec.MaxExecutors = 0, 0
	args := strings.Join(DriverArgs(r, testCluster(), APIEndpoint{IP: "1.2.3.4", Port: 6443}), " ")
	for _, want := range []string{"spark.dynamicAllocation.enabled=false", "spark.executor.instances=0"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %s", want)
		}
	}
	if strings.Contains(args, "maxExecutors=0") {
		t.Error("maxExecutors=0 is passed to Spark, which refuses it")
	}
	r.Spec.MaxExecutors = 2
	if args := strings.Join(DriverArgs(r, testCluster(), APIEndpoint{IP: "1.2.3.4", Port: 6443}), " "); !strings.Contains(args, "spark.dynamicAllocation.enabled=true") {
		t.Error("dynamic allocation is off with executors allowed")
	}
}

// A caller's conf can never override what makes the run isolated.
func TestDriverArgs_CallerConfCannotOverride(t *testing.T) {
	r := testRun(t)
	r.Spec.Conf = map[string]string{"spark.kubernetes.namespace": "kube-system", "spark.ui.killEnabled": "true", "spark.kubernetes.container.image": "evil"}
	args := strings.Join(DriverArgs(r, testCluster(), APIEndpoint{IP: "1.2.3.4", Port: 6443}), " ")
	for _, bad := range []string{"kube-system", "killEnabled=true", "image=evil"} {
		if strings.Contains(args, bad) {
			t.Errorf("caller conf %q reached the driver", bad)
		}
	}
}

func TestBuild_NetworkPolicies(t *testing.T) {
	names := func(c Cluster) map[string]bool {
		out := map[string]bool{}
		for _, p := range build(t, c).NetworkPolicies {
			out[p.Name] = true
		}
		return out
	}
	c := testCluster()
	if got := names(c); !reflect.DeepEqual(got, map[string]bool{"default-deny": true, "same-run": true, "dns": true, "driver-to-api": true, "backend-to-driver": true, "internet": true}) {
		t.Errorf("open: %v", got)
	}
	c.Egress.Mode = "closed"
	if got := names(c); got["internet"] || len(got) != 5 {
		t.Errorf("closed: %v", got)
	}
	pols := map[string]int{}
	o := build(t, testCluster())
	for i, p := range o.NetworkPolicies {
		pols[p.Name] = i
	}
	inet := o.NetworkPolicies[pols["internet"]].Spec.Egress[0].To[0].IPBlock
	if inet.CIDR != "0.0.0.0/0" || !reflect.DeepEqual(inet.Except, []string{"10.0.0.0/8", "192.168.0.0/16"}) {
		t.Errorf("internet rule = %+v", inet)
	}
	api := o.NetworkPolicies[pols["driver-to-api"]]
	if api.Spec.PodSelector.MatchLabels[LabelComponent] != "driver" || api.Spec.Egress[0].To[0].IPBlock.CIDR != "172.19.0.3/32" ||
		api.Spec.Egress[0].Ports[0].Port.IntVal != 6443 {
		t.Errorf("api rule = %+v", api.Spec)
	}
	ui := o.NetworkPolicies[pols["backend-to-driver"]]
	from := ui.Spec.Ingress[0].From[0]
	if from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "booth-spark" ||
		!reflect.DeepEqual(from.PodSelector.MatchLabels, testCluster().BackendPodLabels) || ui.Spec.Ingress[0].Ports[0].Port.IntVal != UIPort ||
		len(ui.Spec.Ingress[0].Ports) != 2 || ui.Spec.Ingress[0].Ports[1].Port.IntVal != SessionPort {
		t.Errorf("ui rule = %+v", ui.Spec)
	}
}

func TestBuild_Quota(t *testing.T) {
	q := build(t, testCluster()).Quota.Spec.Hard
	// Driver 896Mi + 2 executors x 896Mi + 512Mi slack.
	if q.Pods().Value() != 4 || q.Name("limits.memory", "").String() != "3200Mi" || q.Name("limits.cpu", "").String() != "3500m" {
		t.Errorf("quota = %v", q)
	}
}

func TestBuild_NeedsAnAPIEndpoint(t *testing.T) {
	if _, err := Build(testRun(t), testCluster(), nil); err == nil {
		t.Error("built without an API endpoint")
	}
}

func testLimits() Limits {
	return Limits{MaxExecutors: 2, DefaultExecutors: 2, DriverMemory: "512m", ExecutorMemory: "512m", MaxMemory: "2g", MaxDuration: 6 * time.Hour}
}
