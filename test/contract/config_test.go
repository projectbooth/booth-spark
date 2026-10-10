package contract

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/projectbooth/booth-spark/internal/config"
)

// The chart's rendered environment is exactly what the backend's config accepts: every value the
// chart passes parses, with the defaults the design names (docs/design-v0.md item 7). Catches a
// renamed value or a typo on either side.
func TestChart_EnvironmentLoads(t *testing.T) {
	for name, extra := range map[string][]string{
		"defaults": nil,
		"placement and closed egress": {"--set", "runs.driver.nodeSelector.pool=compute", "--set", "runs.egress.mode=closed",
			"--set-json", `runs.executor.tolerations=[{"key":"pool","operator":"Equal","value":"compute","effect":"NoSchedule"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			var dep struct {
				Spec struct {
					Template struct {
						Spec struct {
							Containers []struct {
								Env []struct {
									Name  string `yaml:"name"`
									Value string `yaml:"value"`
								} `yaml:"env"`
							} `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal(helmTemplate(t, "templates/deployment.yaml", extra...), &dep); err != nil {
				t.Fatal(err)
			}
			for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
				t.Setenv(e.Name, e.Value)
			}
			t.Setenv("BOOTH_POSTGRES_DSN", "postgres://x") // from a Secret in the chart
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("the chart's environment doesn't load: %v", err)
			}
			r := cfg.Runs
			if r == nil {
				t.Fatal("no runs configuration")
			}
			if r.Instance != "booth-spark.booth-spark" || r.Namespace != "booth-spark" || r.ServiceAccount != "booth-spark" ||
				r.DriverClusterRole != "booth-spark-driver" || r.RunControllerClusterRole != "booth-spark-run-controller" ||
				r.BackendPodLabels["app.kubernetes.io/name"] != "booth-spark" {
				t.Errorf("identity = %+v", r)
			}
			if r.MaxRunning != 2 || r.MaxRunningPerWorkspace != 1 || r.MemoryBudgetMi != 4096 || r.MaxExecutors != 2 ||
				r.MaxDurationD.String() != "6h0m0s" || r.Driver.Memory != "512m" {
				t.Errorf("limits = %+v", r)
			}
			if name == "defaults" && (r.Egress.Mode != "open" || len(r.Egress.ExceptCIDRs) != 9) {
				t.Errorf("egress = %+v (ADR 0110: open by default, internet only)", r.Egress)
			}
			if name != "defaults" && (r.Egress.Mode != "closed" || r.Driver.NodeSelector["pool"] != "compute" || len(r.Executor.Tolerations) != 1) {
				t.Errorf("overrides not applied: %+v", r)
			}
		})
	}
}
