package runs

import (
	"fmt"
	"strconv"

	"github.com/projectbooth/booth-spark/internal/sparkconf"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// Labels booth-spark puts on what it creates. The namespace's LabelInstance value is what both
// admission policies key on (charts/booth-spark/templates/admission.yaml), so it is set only here,
// by the backend, from verified identity; LabelWorkspace follows ADR 0077.
const (
	LabelInstance  = "booth.projectbooth.io/spark-run"
	LabelRun       = "booth.projectbooth.io/run"
	LabelWorkspace = "booth.projectbooth.io/workspace"
	LabelComponent = "booth.projectbooth.io/component"
	LabelModule    = "booth.projectbooth.io/module"

	NamespacePrefix = "bspark-"

	// Names inside a run's namespace.
	DriverAccount       = "driver"
	ExecutorAccount     = "executor"
	DriverPod           = "driver"
	DriverService       = "driver"
	AppConfigMap        = "app"
	ControllerBinding   = "booth-spark-controller"
	DriverBinding       = "driver"
	DriverRPCPort       = 7078
	BlockManagerPort    = 7079
	UIPort              = 4040
	runMountPath        = "/opt/booth/run"
	localDir            = "/var/data/spark-local"
	executorTemplateKey = "executor-template.yaml"
	mainKey             = "main.py"
)

// Placement is a pod kind's scheduling settings, the plain Kubernetes names (docs/design-v0.md
// item 8).
type Placement struct {
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`
}

// CPU is a pod kind's CPU request and limit.
type CPU struct {
	Request string `json:"request"`
	Limit   string `json:"limit"`
}

// Egress is the runs' internet egress (ADR 0110: open by default, internet only).
type Egress struct {
	Mode string `json:"mode"` // "open" or "closed"
	// ExceptCIDRs are never reachable through the internet rule: every private, CGNAT,
	// link-local, loopback and reserved range, which covers the cluster's pod and Service ranges
	// on k3s and kind, and the cloud metadata address.
	ExceptCIDRs []string `json:"exceptCidrs"`
	DNS         struct {
		NamespaceSelector map[string]string `json:"namespaceSelector"`
		PodSelector       map[string]string `json:"podSelector"`
	} `json:"dns"`
}

// Cluster is everything about the install the objects depend on.
type Cluster struct {
	Instance                 string
	ReleaseNamespace         string
	BackendServiceAccount    string
	BackendPodLabels         map[string]string
	DriverClusterRole        string
	DriverClusterRoleUID     types.UID
	RunControllerClusterRole string
	Image                    string
	ImagePullPolicy          corev1.PullPolicy
	Driver, Executor         Placement
	DriverCPU, ExecutorCPU   CPU
	Egress                   Egress
}

// APIEndpoint is one address of the Kubernetes API server, as the "kubernetes" EndpointSlice in
// "default" lists it. The driver reaches the API at an endpoint, not the Service ClusterIP, so
// its NetworkPolicy can name exactly that address.
type APIEndpoint struct {
	IP   string
	Port int32
}

// Objects is everything a run's namespace holds, in creation order.
type Objects struct {
	Namespace         *corev1.Namespace
	ControllerBinding *rbacv1.RoleBinding
	DriverAccount     *corev1.ServiceAccount
	ExecutorAccount   *corev1.ServiceAccount
	DriverBinding     *rbacv1.RoleBinding
	Quota             *corev1.ResourceQuota
	LimitRange        *corev1.LimitRange
	NetworkPolicies   []*networkingv1.NetworkPolicy
	AppConfigMap      *corev1.ConfigMap
	DriverService     *corev1.Service
	DriverPod         *corev1.Pod
}

func ptr[T any](v T) *T { return &v }

// Build returns every object of run r's namespace. It is pure: same inputs, same objects.
func Build(r Run, c Cluster, api []APIEndpoint) (Objects, error) {
	if len(api) == 0 {
		return Objects{}, fmt.Errorf("no Kubernetes API endpoint known")
	}
	ns := r.Namespace
	v := r.Spec
	runLabels := map[string]string{LabelModule: "spark", LabelWorkspace: r.Workspace, LabelRun: r.ID}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, val := range runLabels {
			out[k] = val
		}
		for k, val := range extra {
			out[k] = val
		}
		return out
	}
	driverLabels := with(map[string]string{LabelComponent: "driver"})
	executorLabels := with(map[string]string{LabelComponent: "executor"})
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: with(nil)}
	}

	o := Objects{}
	o.Namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: ns,
		Labels: map[string]string{
			LabelInstance: c.Instance, LabelWorkspace: r.Workspace, LabelRun: r.ID, LabelModule: "spark",
			"pod-security.kubernetes.io/enforce":         "restricted",
			"pod-security.kubernetes.io/enforce-version": "latest",
		},
		// Owned by the chart's driver ClusterRole: uninstalling the chart, by any route, deletes
		// it, and with it every run namespace, through Kubernetes' garbage collector. No
		// blockOwnerDeletion: that would need update on the owner's finalizers.
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole",
			Name: c.DriverClusterRole, UID: c.DriverClusterRoleUID,
		}},
	}}
	o.ControllerBinding = &rbacv1.RoleBinding{
		ObjectMeta: meta(ControllerBinding),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: c.RunControllerClusterRole},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: c.BackendServiceAccount, Namespace: c.ReleaseNamespace}},
	}
	o.DriverAccount = &corev1.ServiceAccount{ObjectMeta: meta(DriverAccount)}
	o.ExecutorAccount = &corev1.ServiceAccount{ObjectMeta: meta(ExecutorAccount), AutomountServiceAccountToken: ptr(false)}
	o.DriverBinding = &rbacv1.RoleBinding{
		ObjectMeta: meta(DriverBinding),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: c.DriverClusterRole},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: DriverAccount, Namespace: ns}},
	}

	driverMi := PodMemoryMi(v.DriverHeapMi)
	execMi := PodMemoryMi(v.ExecutorHeapMi)
	driverLimit := resource.MustParse(c.DriverCPU.Limit)
	execLimit := resource.MustParse(c.ExecutorCPU.Limit)
	cpuLimits := driverLimit.DeepCopy()
	for i := 0; i < v.MaxExecutors; i++ {
		cpuLimits.Add(execLimit)
	}
	// Slack for one pod the driver creates beyond its executors (a replacement executor, or user
	// code's own pod in the run's own namespace, bounded by the LimitRange below).
	cpuLimits.Add(resource.MustParse("500m"))
	o.Quota = &corev1.ResourceQuota{ObjectMeta: meta("run"), Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
		corev1.ResourcePods:         *resource.NewQuantity(int64(v.MaxExecutors+2), resource.DecimalSI),
		corev1.ResourceLimitsCPU:    cpuLimits,
		corev1.ResourceLimitsMemory: resource.MustParse(strconv.Itoa(driverMi+v.MaxExecutors*execMi+512) + "Mi"),
		// Nothing in a run needs these; the driver can't create Services or claims anyway.
		"count/services":               resource.MustParse("1"),
		"count/persistentvolumeclaims": resource.MustParse("0"),
	}}}
	o.LimitRange = &corev1.LimitRange{ObjectMeta: meta("run"), Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
		Type:           corev1.LimitTypeContainer,
		Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
	}}}}
	o.NetworkPolicies = networkPolicies(ns, c, api, with(nil))

	tmpl, err := executorTemplate(c, executorLabels)
	if err != nil {
		return Objects{}, err
	}
	o.AppConfigMap = &corev1.ConfigMap{ObjectMeta: meta(AppConfigMap), Data: map[string]string{
		mainKey:             v.Main.InlinePython,
		executorTemplateKey: tmpl,
	}}
	o.DriverService = &corev1.Service{ObjectMeta: meta(DriverService), Spec: corev1.ServiceSpec{
		ClusterIP: corev1.ClusterIPNone,
		Selector:  map[string]string{LabelRun: r.ID, LabelComponent: "driver"},
		Ports: []corev1.ServicePort{
			{Name: "rpc", Port: DriverRPCPort, TargetPort: intstr.FromInt32(DriverRPCPort)},
			{Name: "blockmanager", Port: BlockManagerPort, TargetPort: intstr.FromInt32(BlockManagerPort)},
			{Name: "ui", Port: UIPort, TargetPort: intstr.FromInt32(UIPort)},
		},
	}}
	o.DriverPod = driverPod(r, c, api[0], driverLabels, driverMi)
	return o, nil
}

func restrictedPodSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(185)), RunAsGroup: ptr(int64(185)), FSGroup: ptr(int64(185)),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// scratchVolumes are the writable directories of a run's pods (their root filesystems are read
// only): /tmp, which is also spark.local.dir, and Spark's work dir.
func scratchVolumes() ([]corev1.Volume, []corev1.VolumeMount) {
	empty := func() corev1.VolumeSource {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("1Gi"))}}
	}
	vols := []corev1.Volume{{Name: "tmp", VolumeSource: empty()}, {Name: "work", VolumeSource: empty()}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "work", MountPath: "/opt/spark/work-dir"}}
	return vols, mounts
}

// executorTemplate is Spark's executor pod template (spark.kubernetes.executor.podTemplateFile):
// the executor account with no token, the run's labels, the executor placement, and the same
// hardening as the driver. Spark fills in the container's image, command, resources and env.
func executorTemplate(c Cluster, labels map[string]string) (string, error) {
	vols, mounts := scratchVolumes()
	sc := restrictedContainer()
	pod := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			ServiceAccountName:           ExecutorAccount,
			AutomountServiceAccountToken: ptr(false),
			SecurityContext:              restrictedPodSecurity(),
			NodeSelector:                 c.Executor.NodeSelector,
			Tolerations:                  c.Executor.Tolerations,
			Affinity:                     c.Executor.Affinity,
			Volumes:                      vols,
			Containers:                   []corev1.Container{{Name: "executor", SecurityContext: sc, VolumeMounts: mounts}},
		},
	}
	out, err := yaml.Marshal(pod)
	return string(out), err
}

// DriverArgs is the driver container's command: Spark's own spark-submit in client mode, with
// every setting that makes the run isolated and bounded fixed by the module (docs/design-v0.md
// items 1, 3, 5 and 7), then the caller's allowlisted conf, then the application.
func DriverArgs(r Run, c Cluster, api APIEndpoint) []string {
	v := r.Spec
	conf := map[string]string{
		"spark.kubernetes.namespace":                                r.Namespace,
		"spark.kubernetes.driver.pod.name":                          DriverPod,
		"spark.driver.host":                                         DriverService + "." + r.Namespace + ".svc",
		"spark.driver.bindAddress":                                  "0.0.0.0",
		"spark.driver.port":                                         strconv.Itoa(DriverRPCPort),
		"spark.blockManager.port":                                   strconv.Itoa(BlockManagerPort),
		"spark.ui.port":                                             strconv.Itoa(UIPort),
		"spark.kubernetes.container.image":                          c.Image,
		"spark.kubernetes.container.image.pullPolicy":               string(c.ImagePullPolicy),
		"spark.kubernetes.executor.podTemplateFile":                 runMountPath + "/" + executorTemplateKey,
		"spark.kubernetes.executor.podTemplateContainerName":        "executor",
		"spark.kubernetes.executor.podNamePrefix":                   r.ID,
		"spark.kubernetes.authenticate.executor.serviceAccountName": ExecutorAccount,
		"spark.kubernetes.authenticate.caCertFile":                  "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		"spark.kubernetes.authenticate.oauthTokenFile":              "/var/run/secrets/kubernetes.io/serviceaccount/token",
		"spark.kubernetes.executor.deleteOnTermination":             "true",
		"spark.kubernetes.executor.request.cores":                   c.ExecutorCPU.Request,
		"spark.kubernetes.executor.limit.cores":                     c.ExecutorCPU.Limit,
		"spark.executor.cores":                                      "1",
		"spark.executor.memory":                                     strconv.Itoa(v.ExecutorHeapMi) + "m",
		"spark.driver.memory":                                       strconv.Itoa(v.DriverHeapMi) + "m",
		"spark.dynamicAllocation.enabled":                           "true",
		"spark.dynamicAllocation.shuffleTracking.enabled":           "true",
		"spark.dynamicAllocation.minExecutors":                      strconv.Itoa(v.MinExecutors),
		"spark.dynamicAllocation.initialExecutors":                  strconv.Itoa(v.MinExecutors),
		"spark.dynamicAllocation.maxExecutors":                      strconv.Itoa(v.MaxExecutors),
		"spark.dynamicAllocation.executorIdleTimeout":               "60s",
		// RPC authentication between this run's driver and executors (Spark generates the secret).
		"spark.authenticate": "true",
		// Not /tmp: Spark mounts its own emptyDir at the local dir on every executor, and the
		// executor template already mounts /tmp ("must be unique", found by Integration).
		"spark.local.dir": localDir,
		"spark.jars.ivy":  "/tmp/.ivy2",
	}
	for k, val := range sparkconf.UIConf() {
		conf[k] = val
	}
	for k, val := range v.Conf {
		if AllowedConf[k] {
			conf[k] = val
		}
	}
	args := []string{"/opt/spark/bin/spark-submit",
		"--master", fmt.Sprintf("k8s://https://%s:%d", api.IP, api.Port),
		"--deploy-mode", "client",
		"--name", r.ID,
	}
	args = append(args, sparkconf.Args(conf)...)
	args = append(args, runMountPath+"/"+mainKey)
	return append(args, v.Args...)
}

func driverPod(r Run, c Cluster, api APIEndpoint, labels map[string]string, memMi int) *corev1.Pod {
	vols, mounts := scratchVolumes()
	vols = append(vols, corev1.Volume{Name: "run", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: AppConfigMap},
	}}}, corev1.Volume{Name: "spark-local", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("2Gi"))}}})
	// The executors' local dir is Spark's own emptyDir; the driver's, which the backend creates, is this one.
	mounts = append(mounts, corev1.VolumeMount{Name: "run", MountPath: runMountPath, ReadOnly: true},
		corev1.VolumeMount{Name: "spark-local", MountPath: localDir})
	env := []corev1.EnvVar{{Name: "HOME", Value: "/tmp"}}
	for k, val := range sparkconf.UIEnv(r.ID) {
		env = append(env, corev1.EnvVar{Name: k, Value: val})
	}
	mem := resource.MustParse(strconv.Itoa(memMi) + "Mi")
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: DriverPod, Namespace: r.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			ServiceAccountName: DriverAccount,
			// The driver needs its token: it asks the API for executors (ADR 0110 ruling 3, a
			// documented departure from ADR 0057; the token's rights are this namespace only).
			AutomountServiceAccountToken: ptr(true),
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext:              restrictedPodSecurity(),
			NodeSelector:                 c.Driver.NodeSelector,
			Tolerations:                  c.Driver.Tolerations,
			Affinity:                     c.Driver.Affinity,
			EnableServiceLinks:           ptr(false),
			Volumes:                      vols,
			Containers: []corev1.Container{{
				Name:            "driver",
				Image:           c.Image,
				ImagePullPolicy: c.ImagePullPolicy,
				Args:            DriverArgs(r, c, api),
				Env:             env,
				Ports: []corev1.ContainerPort{
					{Name: "rpc", ContainerPort: DriverRPCPort}, {Name: "blockmanager", ContainerPort: BlockManagerPort},
					{Name: "ui", ContainerPort: UIPort},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(c.DriverCPU.Request), corev1.ResourceMemory: mem},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(c.DriverCPU.Limit), corev1.ResourceMemory: mem},
				},
				SecurityContext: restrictedContainer(),
				VolumeMounts:    mounts,
			}},
		},
	}
}

// networkPolicies fence a run's namespace (docs/design-v0.md item 3; ADR 0110 egress ruling):
// default deny; the run's own pods talk to each other; DNS; the driver, and only the driver,
// reaches the API server's endpoints; the backend's pods, and only they, reach the driver's UI;
// with egress mode "open", the internet, never an excepted (private, cluster) range.
func networkPolicies(ns string, c Cluster, api []APIEndpoint, labels map[string]string) []*networkingv1.NetworkPolicy {
	tcp := ptr(corev1.ProtocolTCP)
	udp := ptr(corev1.ProtocolUDP)
	port := func(p int32, proto *corev1.Protocol) networkingv1.NetworkPolicyPort {
		return networkingv1.NetworkPolicyPort{Protocol: proto, Port: ptr(intstr.FromInt32(p))}
	}
	np := func(name string, sel map[string]string, types []networkingv1.PolicyType) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: sel}, PolicyTypes: types},
		}
	}
	both := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	in := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
	out := []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	driver := map[string]string{LabelComponent: "driver"}

	deny := np("default-deny", nil, both)

	same := np("same-run", nil, both)
	samePeer := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	same.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: samePeer}}
	same.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{To: samePeer}}

	dns := np("dns", nil, out)
	dns.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: c.Egress.DNS.NamespaceSelector},
			PodSelector:       &metav1.LabelSelector{MatchLabels: c.Egress.DNS.PodSelector},
		}},
		Ports: []networkingv1.NetworkPolicyPort{port(53, udp), port(53, tcp)},
	}}

	apiNP := np("driver-to-api", driver, out)
	for _, e := range api {
		apiNP.Spec.Egress = append(apiNP.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: e.IP + "/32"}}},
			Ports: []networkingv1.NetworkPolicyPort{port(e.Port, tcp)},
		})
	}

	ui := np("backend-to-driver-ui", driver, in)
	ui.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": c.ReleaseNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: c.BackendPodLabels},
		}},
		Ports: []networkingv1.NetworkPolicyPort{port(UIPort, tcp)},
	}}

	pols := []*networkingv1.NetworkPolicy{deny, same, dns, apiNP, ui}
	if c.Egress.Mode == "open" {
		inet := np("internet", nil, out)
		inet.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: c.Egress.ExceptCIDRs}}},
		}}
		pols = append(pols, inet)
	}
	return pols
}
