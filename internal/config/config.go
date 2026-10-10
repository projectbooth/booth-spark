// Package config loads booth-spark's runtime configuration from environment variables. Every value
// maps 1:1 to a Helm chart value/env var, mirroring the sibling modules' own internal/config —
// there is no config file format of our own to version.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

// Config is booth-spark's full runtime configuration.
type Config struct {
	// HTTPAddr is the address the HTTP server listens on.
	HTTPAddr string

	// PostgresDSN is this module's own database (run records, from step 3), the one booth-core
	// provisions because the manifest declares `database: {enabled: true}` (ADR 0053) and delivers
	// as the booth-database-credentials Secret's `dsn` key.
	PostgresDSN string

	// OIDC verifies bearer tokens on the published /v1 API (docs/design-v0.md item 2): people's
	// tokens from the deployment's provider and, with WorkloadIssuerURL, booth-core's workload
	// tokens. An empty IssuerURL leaves /v1 answering 503.
	OIDC auth.OIDCConfig

	// IframeIssuerURL is booth-core's iframe-identity issuer (ADR 0069), the only thing the iframe
	// routes trust. Required: without it nobody can open the module's UI.
	IframeIssuerURL string

	// SubmitMinRole is the lowest workspace role that may submit an application or start a session
	// (ADR 0110: chart value submit.minRole, "editor" by default, "owner" allowed). Viewers never
	// submit.
	SubmitMinRole auth.Role

	// Runs is the chart's `runs` values plus the install's identity (BOOTH_RUNS and the
	// BOOTH_INSTANCE etc. variables the chart sets). Nil when BOOTH_RUNS is unset: the module then
	// serves identity and health but no runs (the Go tests' configuration, never a chart install).
	Runs *Runs

	// SessionIdleTimeout stops a session after this long with nothing waiting or running and no
	// new statement (sessions.idleTimeout); a caller may ask for less, never more.
	SessionIdleTimeout time.Duration
	// SessionMaxLifetime stops a session after this long regardless (sessions.maxLifetime).
	SessionMaxLifetime time.Duration
	// SessionResultRetention is how long an ended run's content is kept (sessions.resultRetention).
	SessionResultRetention time.Duration

	// Data is the runs' data access (dataAccess, docs/design-v0.md item 4). Nil: off.
	Data *DataAccess
}

// Runs configures the run controller (docs/design-v0.md items 3, 7 and 8).
type Runs struct {
	Image                  string      `json:"image"`
	ImagePullPolicy        string      `json:"imagePullPolicy"`
	MaxRunning             int         `json:"maxRunning"`
	MaxRunningPerWorkspace int         `json:"maxRunningPerWorkspace"`
	MemoryBudget           string      `json:"memoryBudget"`
	MaxExecutors           int         `json:"maxExecutors"`
	DefaultExecutors       int         `json:"defaultExecutors"`
	MaxMemory              string      `json:"maxMemory"`
	MaxDuration            string      `json:"maxDuration"`
	PendingTimeout         string      `json:"pendingTimeout"`
	Driver                 PodKind     `json:"driver"`
	Executor               PodKind     `json:"executor"`
	Egress                 runs.Egress `json:"egress"`

	// Set from the chart's own names, not from runs values.
	Instance                 string            `json:"-"`
	Namespace                string            `json:"-"`
	ServiceAccount           string            `json:"-"`
	BackendPodLabels         map[string]string `json:"-"`
	DriverClusterRole        string            `json:"-"`
	RunControllerClusterRole string            `json:"-"`

	// Parsed.
	MemoryBudgetMi  int           `json:"-"`
	MaxDurationD    time.Duration `json:"-"`
	PendingTimeoutD time.Duration `json:"-"`
}

// PodKind is a driver's or an executor's defaults and placement.
type PodKind struct {
	Memory string   `json:"memory"`
	CPU    runs.CPU `json:"cpu"`
	runs.Placement
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getEnv("BOOTH_HTTP_ADDR", ":8080"),
		PostgresDSN: os.Getenv("BOOTH_POSTGRES_DSN"),
		OIDC: auth.OIDCConfig{
			IssuerURL:         os.Getenv("BOOTH_OIDC_ISSUER_URL"),
			ClientID:          os.Getenv("BOOTH_OIDC_CLIENT_ID"),
			GroupsClaim:       getEnv("BOOTH_OIDC_GROUPS_CLAIM", auth.DefaultGroupsClaim),
			JWKSURL:           os.Getenv("BOOTH_OIDC_JWKS_URL"),
			WorkloadIssuerURL: os.Getenv("BOOTH_WORKLOAD_ISSUER_URL"),
		},
		IframeIssuerURL: os.Getenv("BOOTH_IFRAME_IDENTITY_ISSUER_URL"),
		SubmitMinRole:   auth.Role(getEnv("BOOTH_SUBMIT_MIN_ROLE", string(auth.RoleEditor))),
	}
	if cfg.PostgresDSN == "" {
		return Config{}, fmt.Errorf("BOOTH_POSTGRES_DSN is required (the chart sets it from booth-core's booth-database-credentials Secret, ADR 0053)")
	}
	if cfg.IframeIssuerURL == "" {
		return Config{}, fmt.Errorf("BOOTH_IFRAME_IDENTITY_ISSUER_URL is required: the module's UI is authenticated by booth-core's X-Booth-Identity assertion (ADR 0069)")
	}
	if v := os.Getenv("BOOTH_OIDC_REQUIRE_AUDIENCE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("BOOTH_OIDC_REQUIRE_AUDIENCE: %w", err)
		}
		cfg.OIDC.RequireAudience = b
	}
	// ADR 0108: a key URL without an issuer has no `iss` to validate against.
	if cfg.OIDC.JWKSURL != "" && cfg.OIDC.IssuerURL == "" {
		return Config{}, fmt.Errorf("BOOTH_OIDC_JWKS_URL is set but BOOTH_OIDC_ISSUER_URL is empty: the issuer is still required to validate `iss`")
	}
	if cfg.OIDC.WorkloadIssuerURL != "" && cfg.OIDC.IssuerURL == "" {
		return Config{}, fmt.Errorf("BOOTH_WORKLOAD_ISSUER_URL is set but BOOTH_OIDC_ISSUER_URL is empty: /v1 needs the OIDC provider before it trusts a second issuer")
	}
	if cfg.OIDC.IssuerURL != "" && cfg.OIDC.ClientID == "" {
		return Config{}, fmt.Errorf("BOOTH_OIDC_CLIENT_ID is required when BOOTH_OIDC_ISSUER_URL is set")
	}
	if cfg.SubmitMinRole != auth.RoleEditor && cfg.SubmitMinRole != auth.RoleOwner {
		return Config{}, fmt.Errorf("BOOTH_SUBMIT_MIN_ROLE must be editor or owner (viewers never submit, ADR 0110), not %q", cfg.SubmitMinRole)
	}
	for name, d := range map[string]*time.Duration{
		"BOOTH_SESSION_IDLE_TIMEOUT": &cfg.SessionIdleTimeout, "BOOTH_SESSION_MAX_LIFETIME": &cfg.SessionMaxLifetime,
		"BOOTH_SESSION_RESULT_RETENTION": &cfg.SessionResultRetention,
	} {
		def := map[string]string{"BOOTH_SESSION_IDLE_TIMEOUT": "20m", "BOOTH_SESSION_MAX_LIFETIME": "12h",
			"BOOTH_SESSION_RESULT_RETENTION": "168h"}[name]
		v, err := time.ParseDuration(getEnv(name, def))
		if err != nil || v < time.Second {
			return Config{}, fmt.Errorf("%s must be a duration of at least 1s", name)
		}
		*d = v
	}
	if cfg.SessionIdleTimeout > cfg.SessionMaxLifetime {
		return Config{}, fmt.Errorf("BOOTH_SESSION_IDLE_TIMEOUT is longer than BOOTH_SESSION_MAX_LIFETIME")
	}
	if v := os.Getenv("BOOTH_RUNS"); v != "" {
		r, err := loadRuns(v)
		if err != nil {
			return Config{}, fmt.Errorf("BOOTH_RUNS: %w", err)
		}
		cfg.Runs = r
	}
	if v := os.Getenv("BOOTH_DATA_ACCESS"); v != "" {
		d, err := loadData(v)
		if err != nil {
			return Config{}, fmt.Errorf("BOOTH_DATA_ACCESS: %w", err)
		}
		cfg.Data = d
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadRuns(raw string) (*Runs, error) {
	var r Runs
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	r.Instance = os.Getenv("BOOTH_INSTANCE")
	r.Namespace = os.Getenv("BOOTH_NAMESPACE")
	r.ServiceAccount = os.Getenv("BOOTH_SERVICE_ACCOUNT")
	r.DriverClusterRole = os.Getenv("BOOTH_DRIVER_CLUSTERROLE")
	r.RunControllerClusterRole = os.Getenv("BOOTH_RUN_CONTROLLER_CLUSTERROLE")
	if err := json.Unmarshal([]byte(getEnv("BOOTH_BACKEND_POD_LABELS", "{}")), &r.BackendPodLabels); err != nil {
		return nil, fmt.Errorf("BOOTH_BACKEND_POD_LABELS: %w", err)
	}
	for name, v := range map[string]string{
		"BOOTH_INSTANCE": r.Instance, "BOOTH_NAMESPACE": r.Namespace, "BOOTH_SERVICE_ACCOUNT": r.ServiceAccount,
		"BOOTH_DRIVER_CLUSTERROLE": r.DriverClusterRole, "BOOTH_RUN_CONTROLLER_CLUSTERROLE": r.RunControllerClusterRole,
		"runs.image": r.Image,
	} {
		if v == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	if len(r.BackendPodLabels) == 0 {
		return nil, fmt.Errorf("BOOTH_BACKEND_POD_LABELS is required: the run namespaces admit the driver UI only from these pods")
	}
	switch r.ImagePullPolicy {
	case "Always", "IfNotPresent", "Never":
	default:
		return nil, fmt.Errorf("runs.imagePullPolicy %q", r.ImagePullPolicy)
	}
	if r.MaxRunning < 1 || r.MaxRunningPerWorkspace < 0 || r.MaxExecutors < 0 || r.DefaultExecutors < 0 || r.DefaultExecutors > r.MaxExecutors {
		return nil, fmt.Errorf("runs: need maxRunning >= 1, maxRunningPerWorkspace >= 0 and 0 <= defaultExecutors <= maxExecutors")
	}
	q, err := resource.ParseQuantity(r.MemoryBudget)
	if err != nil {
		return nil, fmt.Errorf("runs.memoryBudget: %w", err)
	}
	r.MemoryBudgetMi = int(q.Value() >> 20)
	if r.MaxDurationD, err = time.ParseDuration(r.MaxDuration); err != nil || r.MaxDurationD <= 0 {
		return nil, fmt.Errorf("runs.maxDuration %q", r.MaxDuration)
	}
	if r.PendingTimeoutD, err = time.ParseDuration(r.PendingTimeout); err != nil || r.PendingTimeoutD <= 0 {
		return nil, fmt.Errorf("runs.pendingTimeout %q", r.PendingTimeout)
	}
	for name, m := range map[string]string{"runs.maxMemory": r.MaxMemory, "runs.driver.memory": r.Driver.Memory, "runs.executor.memory": r.Executor.Memory} {
		if _, err := runs.HeapMi(m); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	for name, c := range map[string]runs.CPU{"runs.driver.cpu": r.Driver.CPU, "runs.executor.cpu": r.Executor.CPU} {
		if _, err := resource.ParseQuantity(c.Request); err != nil {
			return nil, fmt.Errorf("%s.request: %w", name, err)
		}
		if _, err := resource.ParseQuantity(c.Limit); err != nil {
			return nil, fmt.Errorf("%s.limit: %w", name, err)
		}
	}
	if r.Egress.Mode != "open" && r.Egress.Mode != "closed" {
		return nil, fmt.Errorf("runs.egress.mode must be open or closed, not %q", r.Egress.Mode)
	}
	for _, c := range r.Egress.ExceptCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return nil, fmt.Errorf("runs.egress.exceptCidrs: %q: %w", c, err)
		}
	}
	if len(r.Egress.DNS.PodSelector) == 0 || len(r.Egress.DNS.NamespaceSelector) == 0 {
		return nil, fmt.Errorf("runs.egress.dns needs a namespaceSelector and a podSelector")
	}
	return &r, nil
}
