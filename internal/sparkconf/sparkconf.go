// Package sparkconf holds the Spark configuration booth-spark sets on every driver it starts, so
// the rules in docs/design-v0.md item 5 live in one place: the Spark UI's kill, thread-dump and
// heap-histogram actions are off in Spark itself (and blocked again by internal/uiproxy), secrets
// are redacted from the Environment page, and the UI's links resolve under the run's iframe prefix.
// Step 3 applies it to the driver pods the backend creates; step 2's proof driver
// (test/integration/fixtures/proof-driver.yaml) uses it too, and a contract test keeps them equal.
package sparkconf

import (
	"sort"

	"github.com/projectbooth/booth-spark/internal/uiproxy"
)

// RedactionRegex extends Spark's default spark.redaction.regex
// ("(?i)secret|password|token|access[.]?key") with the words booth-spark's own settings could use.
// Secrets are passed to drivers as file paths, never as conf values; this is the second line.
const RedactionRegex = "(?i)secret|password|token|access[.]?key|bearer|credential"

// ProxyBaseEnv is how Spark's UI learns its proxy base: UIUtils reads the spark.ui.proxyBase system
// property, else this environment variable (Spark 4.1.3; neither is documented, so step 2 proves it
// through a real core).
const ProxyBaseEnv = "APPLICATION_WEB_PROXY_BASE"

// UIConf is the Spark UI configuration of every booth-spark driver.
func UIConf() map[string]string {
	return map[string]string{
		"spark.ui.killEnabled":          "false",
		"spark.ui.threadDumpsEnabled":   "false",
		"spark.ui.heapHistogramEnabled": "false",
		"spark.redaction.regex":         RedactionRegex,
	}
}

// UIEnv is the environment a run's driver needs for its UI to work behind the iframe proxy.
func UIEnv(runID string) map[string]string {
	return map[string]string{ProxyBaseEnv: uiproxy.BasePath(runID)}
}

// Args renders conf as spark-submit --conf arguments, in a stable order.
func Args(conf map[string]string) []string {
	keys := make([]string, 0, len(conf))
	for k := range conf {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		out = append(out, "--conf", k+"="+conf[k])
	}
	return out
}
