package contract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/projectbooth/booth-spark/internal/config"
)

const runtimeImage = "ghcr.io/projectbooth/booth-spark-runtime@sha256:0000000000000000000000000000000000000000000000000000000000000000"

var dataOn = []string{"--set", "dataAccess.enabled=true", "--set", "runs.image=" + runtimeImage,
	"--set", "dataAccess.database.enabled=true", "--set", "dataAccess.lakehouse.enabled=true", "--set", "dataAccess.storage.enabled=true",
	"--set", "dataAccess.objectStore.egress.podSelector.app=minio", "--set", "dataAccess.objectStore.egress.namespaceSelector.kubernetes\\.io/metadata\\.name=minio"}

// Data access (docs/design-v0.md item 4): off by default; on, the manifest declares minting, the
// backend gets its minting credential, internal port and data values, and the run-pods policy
// admits exactly the runtime image, the backend's own (the agent) and the pinned credential sidecar.
func TestChart_DataAccess(t *testing.T) {
	if m := renderBoothModule(t, dataOn...); !reflect.DeepEqual(m.Spec.WorkloadIdentity, map[string]any{"mint": true}) {
		t.Errorf("workloadIdentity = %v, want {mint: true}", m.Spec.WorkloadIdentity)
	}

	vars := map[string]string{}
	for _, v := range policies(t, dataOn...)["ValidatingAdmissionPolicy/booth-spark-run-pods"].Spec.Variables {
		vars[v.Name] = v.Expression
	}
	var allowed []string
	if err := json.Unmarshal([]byte(vars["allowed"]), &allowed); err != nil {
		t.Fatalf("allowlist %s: %v", vars["allowed"], err)
	}
	want := []string{runtimeImage, "ghcr.io/projectbooth/booth-spark:0.1.0",
		"ghcr.io/projectbooth/credential-sidecar@sha256:6a0a795efd27f165e0714beb163d91f5c2feff55cfdc6f287aee979ae02cce14"}
	if !reflect.DeepEqual(allowed, want) {
		t.Errorf("run-pods images = %v, want exactly %v", allowed, want)
	}
	vars = map[string]string{}
	for _, v := range policies(t)["ValidatingAdmissionPolicy/booth-spark-run-pods"].Spec.Variables {
		vars[v.Name] = v.Expression
	}
	if strings.Contains(vars["allowed"], "credential-sidecar") || strings.Contains(vars["allowed"], "booth-spark:") {
		t.Errorf("with data access off, the allowlist has more than runs.image: %s", vars["allowed"])
	}

	// The backend's environment loads, with every data value the chart passes.
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name      string         `yaml:"name"`
							Value     string         `yaml:"value"`
							ValueFrom map[string]any `yaml:"valueFrom"`
						} `yaml:"env"`
						Ports []struct {
							Name          string `yaml:"name"`
							ContainerPort int    `yaml:"containerPort"`
						} `yaml:"ports"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(helmTemplate(t, "templates/deployment.yaml", dataOn...), &dep); err != nil {
		t.Fatal(err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	fromSecret := map[string]bool{}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			fromSecret[e.Name] = true
			continue
		}
		t.Setenv(e.Name, e.Value)
	}
	for _, k := range []string{"BOOTH_WORKLOAD_MINT_URL", "BOOTH_WORKLOAD_MINT_CREDENTIAL"} {
		if !fromSecret[k] {
			t.Errorf("%s doesn't come from booth-workload-minting-credentials", k)
		}
		t.Setenv(k, "from-core")
	}
	t.Setenv("BOOTH_POSTGRES_DSN", "postgres://x")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("the chart's environment doesn't load: %v", err)
	}
	d := cfg.Data
	if d == nil {
		t.Fatal("no data access configuration")
	}
	if d.AgentImage != "ghcr.io/projectbooth/booth-spark:0.1.0" || d.InternalURL != "http://booth-spark.booth-spark.svc:8081" || d.InternalPort != 8081 ||
		d.CoreURL != "http://booth-core.booth-system.svc:8080" || !d.Database.Enabled || !d.Lakehouse.Enabled || !d.Storage.Enabled ||
		d.Database.Egress.Port != 5432 || d.ObjectStore.Egress.PodSelector["app"] != "minio" {
		t.Errorf("data config = %+v", d)
	}
	ports := map[string]int{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports["internal"] != 8081 {
		t.Errorf("container ports = %v", ports)
	}

	// The internal port: this install's run namespaces only.
	var np struct {
		Spec struct {
			PolicyTypes []string `yaml:"policyTypes"`
			Ingress     []struct {
				From []struct {
					NamespaceSelector struct {
						MatchLabels map[string]string `yaml:"matchLabels"`
					} `yaml:"namespaceSelector"`
				} `yaml:"from"`
				Ports []struct {
					Port int `yaml:"port"`
				} `yaml:"ports"`
			} `yaml:"ingress"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(helmTemplate(t, "templates/networkpolicy.yaml", dataOn...), &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Ingress) != 2 || np.Spec.Ingress[1].Ports[0].Port != 8081 || len(np.Spec.Ingress[1].From) != 1 ||
		!reflect.DeepEqual(np.Spec.Ingress[1].From[0].NamespaceSelector.MatchLabels, map[string]string{"booth.projectbooth.io/spark-run": "booth-spark.booth-spark"}) ||
		len(np.Spec.Ingress[0].From) != 0 || np.Spec.Ingress[0].Ports[0].Port != 8080 {
		t.Errorf("backend ingress = %+v", np.Spec)
	}

	// Refused: data access with plain Spark (no JDBC, Iceberg or S3A), or an unpinned sidecar.
	helmFails(t, "needs runs.image to be booth-spark's runtime image", "--api-versions", vapAPI, "--set", "dataAccess.enabled=true")
	helmFails(t, "must be pinned by digest", "--api-versions", vapAPI, "--set", "dataAccess.enabled=true", "--set", "runs.image="+runtimeImage,
		"--set", "dataAccess.sidecar.image=ghcr.io/projectbooth/credential-sidecar:latest")
}
