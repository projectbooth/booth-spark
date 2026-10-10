package contract

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type rule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	Verbs         []string `yaml:"verbs"`
	ResourceNames []string `yaml:"resourceNames"`
}

type rbacObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules   []rule `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
}

// flatten turns rules into sorted "group/resource[names]:verb" lines, so two rule lists compare
// equal exactly when they grant the same thing.
func flatten(rules []rule) []string {
	var out []string
	for _, r := range rules {
		names := ""
		if len(r.ResourceNames) > 0 {
			n := append([]string(nil), r.ResourceNames...)
			sort.Strings(n)
			names = "[" + strings.Join(n, ",") + "]"
		}
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					out = append(out, g+"/"+res+names+":"+v)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func rbacObjects(t *testing.T) map[string]rbacObject {
	t.Helper()
	out := map[string]rbacObject{}
	for _, d := range splitDocs(t, helmTemplate(t, "templates/rbac.yaml")) {
		var o rbacObject
		if err := yaml.Unmarshal([]byte(d), &o); err != nil {
			t.Fatal(err)
		}
		out[o.Kind+"/"+o.Metadata.Name] = o
	}
	return out
}

func splitDocs(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for _, d := range strings.Split(string(b), "\n---\n") {
		if strings.Contains(d, "kind:") {
			out = append(out, d)
		}
	}
	return out
}

func expand(lines ...string) []string {
	var out []string
	for _, l := range lines {
		// "group/res1,res2[names]:v1,v2"
		gr, verbs, _ := strings.Cut(l, ":")
		group, rest, _ := strings.Cut(gr, "/")
		names := ""
		if i := strings.Index(rest, "["); i >= 0 {
			names, rest = rest[i:], rest[:i]
		}
		for _, res := range strings.Split(rest, ",") {
			for _, v := range strings.Split(verbs, ",") {
				out = append(out, group+"/"+res+names+":"+v)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ADR 0110 step 3, requirement 5: every rule booth-spark holds or grants is exactly the design's
// (docs/design-v0.md item 3, charts/booth-spark/templates/rbac.yaml). A rule added or widened
// anywhere fails here first.
func TestChart_RBACIsExactlyTheDesign(t *testing.T) {
	objs := rbacObjects(t)
	want := map[string][]string{
		// Cluster-wide, fenced by the "fence" admission policy.
		"ClusterRole/booth-spark-controller": expand(
			"/namespaces:create,delete,get,list",
			"rbac.authorization.k8s.io/rolebindings:create",
			"rbac.authorization.k8s.io/clusterroles[booth-spark-driver,booth-spark-run-controller]:bind",
			"rbac.authorization.k8s.io/clusterroles[booth-spark-driver]:get",
		),
		// One object in default: the API server's address.
		"Role/booth-spark-api-endpoints": expand("discovery.k8s.io/endpointslices[kubernetes]:get"),
		// Bound by the backend inside each run namespace, only there.
		"ClusterRole/booth-spark-run-controller": expand(
			"/serviceaccounts,configmaps,services,resourcequotas,limitranges,secrets:create",
			"/pods:create,get",
			"/pods/log:get",
			"networking.k8s.io/networkpolicies:create",
			"rbac.authorization.k8s.io/rolebindings:create",
		),
		// The driver's, in its own run namespace only, measured against Spark 4.1.3.
		"ClusterRole/booth-spark-driver": expand(
			"/pods:create,get,list,watch,delete,deletecollection",
			"/configmaps:create,deletecollection",
			"/services,persistentvolumeclaims:deletecollection",
		),
	}
	var kinds []string
	for k := range objs {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	wantKinds := []string{
		"ClusterRole/booth-spark-controller", "ClusterRole/booth-spark-driver", "ClusterRole/booth-spark-run-controller",
		"ClusterRoleBinding/booth-spark-controller", "Role/booth-spark-api-endpoints", "RoleBinding/booth-spark-api-endpoints",
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("RBAC objects = %v, want exactly %v", kinds, wantKinds)
	}
	for name, rules := range want {
		if got := flatten(objs[name].Rules); !reflect.DeepEqual(got, rules) {
			t.Errorf("%s grants\n  %v\nwant exactly\n  %v", name, got, rules)
		}
	}
	// Nothing the backend holds is cluster-wide pods, secrets or pods/log (ADR 0110 ruling 2).
	for _, l := range flatten(objs["ClusterRole/booth-spark-controller"].Rules) {
		for _, banned := range []string{"/pods:", "/pods/log:", "/secrets:"} {
			if strings.Contains(l, banned) {
				t.Errorf("the backend's cluster-wide role grants %s", l)
			}
		}
	}
	// No rule anywhere reads Secrets (the run controller may only create a session's bearer, step 4),
	// or touches exec, attach, port-forward, or uses a wildcard.
	for name, o := range objs {
		for _, l := range flatten(o.Rules) {
			if (strings.Contains(l, "secrets") && l != "/secrets:create") || strings.Contains(l, "pods/exec") || strings.Contains(l, "pods/attach") ||
				strings.Contains(l, "pods/portforward") || strings.Contains(l, "*") {
				t.Errorf("%s grants %s", name, l)
			}
		}
	}
	// Bindings: the backend's account, in the release namespace, and nothing else.
	for _, name := range []string{"ClusterRoleBinding/booth-spark-controller", "RoleBinding/booth-spark-api-endpoints"} {
		b := objs[name]
		if len(b.Subjects) != 1 || b.Subjects[0].Kind != "ServiceAccount" || b.Subjects[0].Name != "booth-spark" || b.Subjects[0].Namespace != "booth-spark" {
			t.Errorf("%s subjects = %+v, want only the backend's ServiceAccount", name, b.Subjects)
		}
	}
	if b := objs["RoleBinding/booth-spark-api-endpoints"]; b.Metadata.Namespace != "default" || b.RoleRef.Name != "booth-spark-api-endpoints" {
		t.Errorf("api-endpoints binding = %+v", b)
	}
	if b := objs["ClusterRoleBinding/booth-spark-controller"]; b.RoleRef.Kind != "ClusterRole" || b.RoleRef.Name != "booth-spark-controller" {
		t.Errorf("controller binding = %+v", b)
	}
}

// The backend pod mounts its token now (it calls the API with exactly the rights above); nothing
// else in the chart does, and it still runs hardened.
func TestChart_BackendRunsHardened(t *testing.T) {
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	for _, want := range []string{"automountServiceAccountToken: true", "runAsNonRoot: true", "readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false", "type: RuntimeDefault", "- ALL"} {
		if !strings.Contains(dep, want) {
			t.Errorf("deployment lacks %q", want)
		}
	}
}

type vap struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		FailurePolicy     string   `yaml:"failurePolicy"`
		PolicyName        string   `yaml:"policyName"`
		ValidationActions []string `yaml:"validationActions"`
		MatchConstraints  struct {
			NamespaceSelector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"namespaceSelector"`
			ResourceRules []struct {
				Resources  []string `yaml:"resources"`
				Operations []string `yaml:"operations"`
			} `yaml:"resourceRules"`
		} `yaml:"matchConstraints"`
		MatchConditions []struct {
			Expression string `yaml:"expression"`
		} `yaml:"matchConditions"`
		Variables []struct {
			Name       string `yaml:"name"`
			Expression string `yaml:"expression"`
		} `yaml:"variables"`
		Validations []struct {
			Expression string `yaml:"expression"`
		} `yaml:"validations"`
	} `yaml:"spec"`
}

func policies(t *testing.T, extra ...string) map[string]vap {
	t.Helper()
	out := map[string]vap{}
	for _, d := range splitDocs(t, helmTemplate(t, "templates/admission.yaml", extra...)) {
		var p vap
		if err := yaml.Unmarshal([]byte(d), &p); err != nil {
			t.Fatal(err)
		}
		out[p.Kind+"/"+p.Metadata.Name] = p
	}
	return out
}

// Both admission policies ship, fail closed, are bound with Deny, and are keyed on this install's
// own label value and backend identity (docs/design-v0.md item 3).
func TestChart_AdmissionPolicies(t *testing.T) {
	ps := policies(t, "--set", "runs.driver.nodeSelector.pool=compute", "--set", "runs.executor.nodeSelector.pool=compute")
	pods := ps["ValidatingAdmissionPolicy/booth-spark-run-pods"]
	fence := ps["ValidatingAdmissionPolicy/booth-spark-fence"]
	for name, p := range map[string]vap{"run-pods": pods, "fence": fence} {
		if p.Spec.FailurePolicy != "Fail" || len(p.Spec.Validations) == 0 {
			t.Errorf("%s: failurePolicy %q, %d validations", name, p.Spec.FailurePolicy, len(p.Spec.Validations))
		}
		b := ps["ValidatingAdmissionPolicyBinding/booth-spark-"+name]
		if b.Spec.PolicyName != "booth-spark-"+name || !reflect.DeepEqual(b.Spec.ValidationActions, []string{"Deny"}) {
			t.Errorf("%s binding = %+v", name, b.Spec)
		}
	}
	if got := pods.Spec.MatchConstraints.NamespaceSelector.MatchLabels; !reflect.DeepEqual(got, map[string]string{"booth.projectbooth.io/spark-run": "booth-spark.booth-spark"}) {
		t.Errorf("run-pods selects namespaces %v", got)
	}
	vars := map[string]string{}
	for _, v := range pods.Spec.Variables {
		vars[v.Name] = v.Expression
	}
	if !strings.Contains(vars["allowed"], `"apache/spark:4.1.3-scala2.13-java21-python3-ubuntu@sha256:bf9d035a7c32a8ca46aa58d6348182ffd7d2dff6409206ecfbb3915ff1fef211"`) {
		t.Errorf("image allowlist = %s", vars["allowed"])
	}
	if !strings.Contains(vars["want"], `{"pool":"compute"}`) {
		t.Errorf("nodeSelector rule = %s", vars["want"])
	}
	all := ""
	for _, v := range pods.Spec.Validations {
		all += v.Expression + "\n"
	}
	for _, want := range []string{`request.userInfo.username == "system:serviceaccount:booth-spark:booth-spark"`, "'executor'", "automountServiceAccountToken == false",
		"booth.projectbooth.io/workspace"} {
		if !strings.Contains(all, want) {
			t.Errorf("run-pods validations lack %s", want)
		}
	}
	if len(fence.Spec.MatchConditions) != 1 || !strings.Contains(fence.Spec.MatchConditions[0].Expression, `"system:serviceaccount:booth-spark:booth-spark"`) {
		t.Errorf("fence matches %+v, want the backend's account only", fence.Spec.MatchConditions)
	}
	fall := ""
	for _, v := range fence.Spec.Validations {
		fall += v.Expression + "\n"
	}
	for _, want := range []string{"startsWith('bspark-')", `"booth-spark-driver"`, `"booth-spark-run-controller"`, "'restricted'", "request.operation != 'UPDATE'"} {
		if !strings.Contains(fall, want) {
			t.Errorf("fence validations lack %s", want)
		}
	}
}
