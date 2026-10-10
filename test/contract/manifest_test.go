// Package contract validates booth-spark's own manifest (the BoothModule custom resource its Helm
// chart templates) against contracts/module-manifest.md, and checks the chart's wiring, its
// install-time checks (ADR 0110) and the repo's pinning rules (ADR 0099, digest-pinned base
// images). Per contracts/testing-strategy.md this runs against a rendered template (`helm
// template`), not a deployed cluster: no cluster needed, but a `helm` binary is, which ci.yml
// installs. Locally, a missing helm skips these tests; in CI it fails them.
package contract

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type boothModule struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		ID                string   `yaml:"id"`
		DisplayName       string   `yaml:"displayName"`
		Icon              string   `yaml:"icon"`
		Version           string   `yaml:"version"`
		ContractVersion   string   `yaml:"contractVersion"`
		HasOwnUI          bool     `yaml:"hasOwnUi"`
		UIIntegrationMode string   `yaml:"uiIntegrationMode"`
		HealthCheckPath   string   `yaml:"healthCheckPath"`
		RequiredScopes    []string `yaml:"requiredScopes"`
		NavGroup          string   `yaml:"navGroup"`
		NavPath           string   `yaml:"navPath"`
		AdminNavPath      string   `yaml:"adminNavPath"`
		Database          *struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"database"`
		Events           map[string]any `yaml:"events"`
		WorkloadIdentity map[string]any `yaml:"workloadIdentity"`
		PublicRoutes     map[string]any `yaml:"publicRoutes"`
		ServiceRef       struct {
			Name string `yaml:"name"`
			Port int    `yaml:"port"`
		} `yaml:"serviceRef"`
	} `yaml:"spec"`
}

// vapAPI is what a live cluster's discovery lists when it serves ValidatingAdmissionPolicy. Helm's
// offline `template` doesn't list kind-level APIs, so the renders here say so explicitly, exactly as
// a real 1.30+ cluster would; TestChart_RefusesAClusterWithoutValidatingAdmissionPolicy renders
// without it.
const vapAPI = "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"

func helm(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not installed, but CI must run the contract tests")
		}
		t.Skip("helm not installed; these contract tests run in CI, where it is")
	}
	return exec.Command("helm", args...).CombinedOutput()
}

func repoFile(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func chartDir() string { return repoFile("charts", "booth-spark") }

func helmTemplate(t *testing.T, showOnly string, extra ...string) []byte {
	t.Helper()
	args := []string{"template", "booth-spark", chartDir(), "--namespace", "booth-spark", "--api-versions", vapAPI}
	args = append(args, extra...)
	if showOnly != "" {
		args = append(args, "--show-only", showOnly)
	}
	out, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// helmFails renders with extra values and requires the render to fail with want in its message.
func helmFails(t *testing.T, want string, extra ...string) {
	t.Helper()
	args := append([]string{"template", "x", chartDir()}, extra...)
	out, err := helm(t, args...)
	if err == nil {
		t.Fatalf("rendered with %v, want a failure mentioning %q:\n%s", extra, want, out)
	}
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("failure message for %v is not actionable, want %q:\n%s", extra, want, out)
	}
}

func renderBoothModule(t *testing.T, extra ...string) boothModule {
	t.Helper()
	var m boothModule
	out := helmTemplate(t, "templates/boothmodule.yaml", extra...)
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("parsing rendered BoothModule: %v\n%s", err, out)
	}
	return m
}

// TestManifest_RequiredFields checks every field contracts/module-manifest.md marks required.
func TestManifest_RequiredFields(t *testing.T) {
	m := renderBoothModule(t)
	if m.APIVersion != "booth.projectbooth.io/v1alpha1" || m.Kind != "BoothModule" {
		t.Errorf("apiVersion/kind = %s/%s, want booth.projectbooth.io/v1alpha1 BoothModule (ADR 0019)", m.APIVersion, m.Kind)
	}
	// The manifest contract: id "matches the repo name minus booth-".
	if m.Spec.ID != "spark" {
		t.Errorf("spec.id = %q, want spark", m.Spec.ID)
	}
	if m.Spec.DisplayName == "" {
		t.Error("spec.displayName is required but empty")
	}
	semver := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	if !semver.MatchString(m.Spec.Version) || !semver.MatchString(m.Spec.ContractVersion) {
		t.Errorf("version=%q contractVersion=%q, want semver", m.Spec.Version, m.Spec.ContractVersion)
	}
	if m.Spec.HealthCheckPath == "" || m.Spec.HealthCheckPath[0] != '/' {
		t.Errorf("spec.healthCheckPath = %q, want a URL path", m.Spec.HealthCheckPath)
	}
	if m.Spec.ServiceRef.Name != "booth-spark" || m.Spec.ServiceRef.Port != 8080 {
		t.Errorf("spec.serviceRef = %+v, want the chart's own Service (booth-spark:8080)", m.Spec.ServiceRef)
	}
}

// TestManifest_UI checks the "required if hasOwnUi" rules and the placement the brief and
// contracts/ui-integration.md call for.
func TestManifest_UI(t *testing.T) {
	m := renderBoothModule(t)
	if !m.Spec.HasOwnUI {
		t.Fatal("spec.hasOwnUi = false, want true")
	}
	if m.Spec.UIIntegrationMode != "iframe-proxy" {
		t.Errorf("spec.uiIntegrationMode = %q, want iframe-proxy (ui-integration.md: Spark's own UI)", m.Spec.UIIntegrationMode)
	}
	if m.Spec.NavGroup != "manage" {
		t.Errorf("spec.navGroup = %q, want manage (agent brief; ADR 0017)", m.Spec.NavGroup)
	}
	if m.Spec.NavPath != "/spark" {
		t.Errorf("spec.navPath = %q, want /spark", m.Spec.NavPath)
	}
	if m.Spec.AdminNavPath != "" {
		t.Errorf("spec.adminNavPath = %q; this module has no admin-only view", m.Spec.AdminNavPath)
	}
}

// Least privilege for the scaffold: nothing is declared that this step doesn't use. Minting
// (workloadIdentity) arrives with data access (step 5); there is no event-bus use and no public
// route at all (ADR 0110: Spark is used through Booth only).
func TestManifest_DeclaresNothingItDoesNotUse(t *testing.T) {
	m := renderBoothModule(t)
	if m.Spec.Events != nil {
		t.Errorf("spec.events = %v; booth-spark uses no event bus", m.Spec.Events)
	}
	if m.Spec.WorkloadIdentity != nil {
		t.Errorf("spec.workloadIdentity = %v; minting arrives with data access (step 5)", m.Spec.WorkloadIdentity)
	}
	if m.Spec.PublicRoutes != nil {
		t.Errorf("spec.publicRoutes = %v; no unauthenticated route is exposed (ADR 0110)", m.Spec.PublicRoutes)
	}
}

// ADR 0053: core provisions the module's database only when the manifest asks for it.
func TestManifest_AsksCoreForADatabase(t *testing.T) {
	m := renderBoothModule(t)
	if m.Spec.Database == nil || !m.Spec.Database.Enabled {
		t.Fatal("spec.database.enabled is not true: booth-core would provision no database and the pod would never start")
	}
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	if !regexp.MustCompile(`BOOTH_POSTGRES_DSN\s+valueFrom:\s+secretKeyRef:\s+name: booth-database-credentials\s+key: dsn`).MatchString(dep) {
		t.Errorf("the DSN should come from core's booth-database-credentials Secret:\n%s", dep)
	}
}

// With the operator's own database, nothing is asked of core, and a missing Secret name fails the
// render with an actionable message instead of a pod that never starts.
func TestChart_OwnDatabase(t *testing.T) {
	m := renderBoothModule(t, "--set", "postgres.provisionedByCore=false", "--set", "postgres.dsnSecret.name=my-db")
	if m.Spec.Database != nil {
		t.Error("spec.database rendered although the operator supplies the database")
	}
	dep := string(helmTemplate(t, "templates/deployment.yaml", "--set", "postgres.provisionedByCore=false", "--set", "postgres.dsnSecret.name=my-db"))
	if !strings.Contains(dep, "name: my-db") {
		t.Errorf("the operator's Secret is not used:\n%s", dep)
	}
	helmFails(t, "postgres.dsnSecret.name is required", "--api-versions", vapAPI, "--set", "postgres.provisionedByCore=false")
}

// Every spec field this chart renders must exist in booth-core's real BoothModule CRD (vendored
// unchanged from booth-core's chart at the Integration job's CORE_REF into
// test/integration/fixtures). The API server silently prunes unknown fields, so a misspelled or
// not-yet-supported field would vanish without error.
func TestManifest_EveryFieldIsInCoresCRD(t *testing.T) {
	crdBytes, err := os.ReadFile(repoFile("test", "integration", "fixtures", "boothmodule-crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `yaml:"name"`
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties map[string]any `yaml:"properties"`
								Required   []string       `yaml:"required"`
							} `yaml:"spec"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(crdBytes, &crd); err != nil {
		t.Fatal(err)
	}
	var known map[string]any
	var required []string
	for _, v := range crd.Spec.Versions {
		if v.Name == "v1alpha1" {
			known = v.Schema.OpenAPIV3Schema.Properties.Spec.Properties
			required = v.Schema.OpenAPIV3Schema.Properties.Spec.Required
		}
	}
	if len(known) == 0 {
		t.Fatal("no v1alpha1 spec properties found in the vendored CRD")
	}
	var rendered struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(helmTemplate(t, "templates/boothmodule.yaml"), &rendered); err != nil {
		t.Fatal(err)
	}
	var fields []string
	for f := range rendered.Spec {
		fields = append(fields, f)
		if _, ok := known[f]; !ok {
			t.Errorf("spec.%s is not in booth-core's BoothModule CRD and would be pruned on apply", f)
		}
	}
	sort.Strings(fields)
	for _, f := range required {
		if _, ok := rendered.Spec[f]; !ok {
			t.Errorf("booth-core's CRD requires spec.%s, which the chart does not render (rendered: %v)", f, fields)
		}
	}
}

// The path core polls and the readiness probe hit the same endpoint; liveness deliberately doesn't.
func TestManifest_HealthPathMatchesProbe(t *testing.T) {
	m := renderBoothModule(t)
	dep := helmTemplate(t, "templates/deployment.yaml")
	if !regexp.MustCompile(`readinessProbe:\s+httpGet:\s+path: ` + regexp.QuoteMeta(m.Spec.HealthCheckPath) + `\b`).Match(dep) {
		t.Errorf("readinessProbe does not use the manifest's healthCheckPath %q:\n%s", m.Spec.HealthCheckPath, dep)
	}
	if !regexp.MustCompile(`livenessProbe:\s+httpGet:\s+path: /livez\b`).Match(dep) {
		t.Error("livenessProbe should use /livez: restarting the pod can't fix a database outage")
	}
}

// ADR 0110 ruling 2: the chart refuses Kubernetes below 1.30, and a cluster that doesn't serve
// ValidatingAdmissionPolicy, with a message that says why.
func TestChart_RefusesOldKubernetes(t *testing.T) {
	helmFails(t, "kubeVersion: >=1.30.0-0", "--api-versions", vapAPI, "--kube-version", "1.29.0")
	helmTemplate(t, "", "--kube-version", "1.30.0")
}

func TestChart_RefusesAClusterWithoutValidatingAdmissionPolicy(t *testing.T) {
	helmFails(t, "ValidatingAdmissionPolicy API served", "--kube-version", "1.31.0")
}

// docs/design-v0.md item 2: the iframe routes trust core's iframe-identity issuer, spelled exactly
// as core's chart publishes it, and an empty value refuses to render.
func TestChart_IframeIdentityIssuer(t *testing.T) {
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	if !regexp.MustCompile(`BOOTH_IFRAME_IDENTITY_ISSUER_URL\s+value: "http://booth-core\.booth-system\.svc\.cluster\.local:8080/iframe-identity"`).MatchString(dep) {
		t.Errorf("default issuer is not core's exact spelling:\n%s", dep)
	}
	if !regexp.MustCompile(`BOOTH_OIDC_GROUPS_CLAIM\s+value: "groups"`).MatchString(dep) {
		t.Error("default groups claim should be \"groups\", matching booth-core")
	}
	helmFails(t, "identity.issuerUrl is required", "--api-versions", vapAPI, "--set", "identity.issuerUrl=")
}

// docs/design-v0.md item 2: every token-verifying setting the fleet has, each wired to its env var,
// and nothing rendered for an unset optional value.
func TestChart_OIDCSettings(t *testing.T) {
	def := string(helmTemplate(t, "templates/deployment.yaml"))
	for _, absent := range []string{"BOOTH_OIDC_ISSUER_URL", "BOOTH_OIDC_CLIENT_ID", "BOOTH_OIDC_JWKS_URL", "BOOTH_WORKLOAD_ISSUER_URL"} {
		if strings.Contains(def, absent) {
			t.Errorf("%s rendered with its value unset", absent)
		}
	}
	set := string(helmTemplate(t, "templates/deployment.yaml",
		"--set", "oidc.issuerUrl=https://booth.example/realms/booth", "--set", "oidc.clientId=booth-design",
		"--set", "oidc.jwksUrl=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect/certs",
		"--set", "oidc.workloadIssuerUrl=http://booth-core.booth-system.svc:8080", "--set", "oidc.groupsClaim=roles"))
	for env, want := range map[string]string{
		"BOOTH_OIDC_ISSUER_URL":       "https://booth.example/realms/booth",
		"BOOTH_OIDC_CLIENT_ID":        "booth-design",
		"BOOTH_OIDC_REQUIRE_AUDIENCE": "true",
		"BOOTH_OIDC_JWKS_URL":         "http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect/certs",
		"BOOTH_WORKLOAD_ISSUER_URL":   "http://booth-core.booth-system.svc:8080",
		"BOOTH_OIDC_GROUPS_CLAIM":     "roles",
	} {
		if !regexp.MustCompile(env + `\s+value: "` + regexp.QuoteMeta(want) + `"`).MatchString(set) {
			t.Errorf("%s is not %q:\n%s", env, want, set)
		}
	}
	// A key URL without an issuer is still rendered, so the backend refuses to start with a clear
	// message rather than the setting being silently dropped (booth-api's chart does the same).
	orphan := string(helmTemplate(t, "templates/deployment.yaml", "--set", "oidc.jwksUrl=http://k/certs"))
	if !strings.Contains(orphan, "BOOTH_OIDC_JWKS_URL") {
		t.Error("oidc.jwksUrl without oidc.issuerUrl was dropped instead of failing at startup")
	}
	helmFails(t, "oidc.clientId is required", "--api-versions", vapAPI, "--set", "oidc.issuerUrl=https://idp.example")
}

// ADR 0110: the submit floor is a chart value, editor by default, owner allowed, nothing else.
func TestChart_SubmitMinRole(t *testing.T) {
	if dep := string(helmTemplate(t, "templates/deployment.yaml")); !regexp.MustCompile(`BOOTH_SUBMIT_MIN_ROLE\s+value: "editor"`).MatchString(dep) {
		t.Errorf("default submit floor is not editor:\n%s", dep)
	}
	if dep := string(helmTemplate(t, "templates/deployment.yaml", "--set", "submit.minRole=owner")); !regexp.MustCompile(`BOOTH_SUBMIT_MIN_ROLE\s+value: "owner"`).MatchString(dep) {
		t.Error("submit.minRole=owner not rendered")
	}
	helmFails(t, `submit.minRole must be "editor" or "owner"`, "--api-versions", vapAPI, "--set", "submit.minRole=viewer")
}

// docs/design-v0.md item 8: the controller's placement uses plain Kubernetes names, passed through.
func TestChart_Placement(t *testing.T) {
	def := string(helmTemplate(t, "templates/deployment.yaml"))
	for _, k := range []string{"nodeSelector:", "tolerations:", "affinity:"} {
		if strings.Contains(def, k) {
			t.Errorf("%s rendered with nothing set", k)
		}
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					NodeSelector map[string]string `yaml:"nodeSelector"`
					Tolerations  []map[string]any  `yaml:"tolerations"`
					Affinity     map[string]any    `yaml:"affinity"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	out := helmTemplate(t, "templates/deployment.yaml",
		"--set", "nodeSelector.booth\\.projectbooth\\.io/pool=compute",
		"--set-json", `tolerations=[{"key":"booth.projectbooth.io/pool","operator":"Equal","value":"compute","effect":"NoSchedule"}]`,
		"--set-json", `affinity={"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":1,"preference":{"matchExpressions":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}}]}}`)
	if err := yaml.Unmarshal(out, &dep); err != nil {
		t.Fatal(err)
	}
	ps := dep.Spec.Template.Spec
	if ps.NodeSelector["booth.projectbooth.io/pool"] != "compute" || len(ps.Tolerations) != 1 || ps.Tolerations[0]["value"] != "compute" || ps.Affinity["nodeAffinity"] == nil {
		t.Errorf("placement not passed through: %+v", ps)
	}
}

// The scaffold's backend has no Kubernetes API access at all (step 3 adds exactly the fenced rights),
// and runs hardened.
func TestChart_BackendHasNoAPIAccessAndRunsHardened(t *testing.T) {
	all := string(helmTemplate(t, ""))
	for _, kind := range []string{"kind: Role", "kind: ClusterRole", "kind: RoleBinding", "kind: ClusterRoleBinding"} {
		if strings.Contains(all, kind) {
			t.Errorf("the scaffold renders %s; the backend gets no API access before step 3", kind)
		}
	}
	if c := strings.Count(all, "automountServiceAccountToken: false"); c != 2 {
		t.Errorf("automountServiceAccountToken: false appears %d times, want on both the ServiceAccount and the pod", c)
	}
	for _, want := range []string{"runAsNonRoot: true", "readOnlyRootFilesystem: true", "allowPrivilegeEscalation: false", "type: RuntimeDefault", `- ALL`} {
		if !strings.Contains(all, want) {
			t.Errorf("chart lacks %q", want)
		}
	}
	if !regexp.MustCompile(`limits:\s+cpu: 500m\s+memory: 256Mi`).MatchString(all) {
		t.Error("the backend's homelab limits (500m / 256Mi, docs/design-v0.md item 7) are not set")
	}
}

// docs/design-v0.md item 1: every base image is pinned by digest, never by tag alone.
func TestDockerfile_BaseImagesPinnedByDigest(t *testing.T) {
	df, err := os.ReadFile(repoFile("Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	from := regexp.MustCompile(`(?m)^FROM\s+(\S+)`)
	matches := from.FindAllStringSubmatch(string(df), -1)
	if len(matches) == 0 {
		t.Fatal("no FROM lines in the Dockerfile")
	}
	for _, m := range matches {
		if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(m[1]) {
			t.Errorf("FROM %s is not pinned by digest", m[1])
		}
	}
}

// ADR 0099: every workflow job pins its runner to ubuntu-24.04. Every job that pulls from Docker
// Hub (docker build, docker compose, kind) logs in first, with booth-core's guard for forks and
// Dependabot (core PR #11).
func TestWorkflows_PinnedRunnersAndDockerHubLogin(t *testing.T) {
	files, err := filepath.Glob(repoFile(".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	const guard = "(github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) && github.actor != 'dependabot[bot]'"
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var wf struct {
			Jobs map[string]struct {
				RunsOn string           `yaml:"runs-on"`
				Steps  []map[string]any `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, job := range wf.Jobs {
			if job.RunsOn != "ubuntu-24.04" {
				t.Errorf("%s job %s runs-on %q, want ubuntu-24.04 (ADR 0099)", filepath.Base(f), name, job.RunsOn)
			}
			pulls, loginAt, firstPull := false, -1, -1
			for i, s := range job.Steps {
				uses, _ := s["uses"].(string)
				run, _ := s["run"].(string)
				if strings.HasPrefix(uses, "docker/login-action@") {
					loginAt = i
					if s["if"] != guard {
						t.Errorf("%s job %s: docker login guard is %q, want booth-core's %q", filepath.Base(f), name, s["if"], guard)
					}
				}
				if strings.HasPrefix(uses, "helm/kind-action@") || strings.HasPrefix(uses, "docker/build-push-action@") ||
					strings.Contains(run, "docker build") || strings.Contains(run, "docker compose") {
					pulls = true
					if firstPull < 0 {
						firstPull = i
					}
				}
			}
			if pulls && (loginAt < 0 || loginAt > firstPull) {
				t.Errorf("%s job %s pulls from Docker Hub without logging in first", filepath.Base(f), name)
			}
		}
	}
}
