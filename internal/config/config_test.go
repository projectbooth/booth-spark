package config

import (
	"strings"
	"testing"
)

const iframeIssuer = "http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity"

func base(t *testing.T) {
	t.Helper()
	t.Setenv("BOOTH_POSTGRES_DSN", "postgres://u:p@h/db")
	t.Setenv("BOOTH_IFRAME_IDENTITY_ISSUER_URL", iframeIssuer)
}

func TestLoad_RequiresDSN(t *testing.T) {
	t.Setenv("BOOTH_POSTGRES_DSN", "")
	t.Setenv("BOOTH_IFRAME_IDENTITY_ISSUER_URL", iframeIssuer)
	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with no database DSN")
	}
}

func TestLoad_RequiresIframeIssuer(t *testing.T) {
	t.Setenv("BOOTH_POSTGRES_DSN", "postgres://u:p@h/db")
	t.Setenv("BOOTH_IFRAME_IDENTITY_ISSUER_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with no iframe-identity issuer")
	}
}

func TestLoad_Defaults(t *testing.T) {
	base(t)
	t.Setenv("BOOTH_HTTP_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.OIDC.GroupsClaim != "groups" || cfg.OIDC.RequireAudience ||
		cfg.OIDC.IssuerURL != "" || cfg.OIDC.WorkloadIssuerURL != "" || cfg.OIDC.JWKSURL != "" ||
		cfg.SubmitMinRole != "editor" || cfg.IframeIssuerURL != iframeIssuer {
		t.Errorf("cfg = %+v", cfg)
	}
}

// ADR 0108: a key URL needs an issuer to validate `iss` against; unset, nothing changes.
func TestLoad_JWKSURL(t *testing.T) {
	base(t)
	if cfg, err := Load(); err != nil || cfg.OIDC.JWKSURL != "" {
		t.Fatalf("unset: %+v %v", cfg.OIDC, err)
	}
	t.Setenv("BOOTH_OIDC_JWKS_URL", "http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect/certs")
	if _, err := Load(); err == nil {
		t.Error("BOOTH_OIDC_JWKS_URL without BOOTH_OIDC_ISSUER_URL was accepted")
	}
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://booth.example/realms/booth")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "booth-design")
	cfg, err := Load()
	if err != nil || cfg.OIDC.JWKSURL != "http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect/certs" || cfg.OIDC.IssuerURL != "https://booth.example/realms/booth" {
		t.Errorf("set: %+v %v", cfg.OIDC, err)
	}
}

func TestLoad_OIDC(t *testing.T) {
	base(t)
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp.example")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "")
	if _, err := Load(); err == nil {
		t.Error("an issuer with no client id was accepted")
	}
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "booth")
	t.Setenv("BOOTH_OIDC_REQUIRE_AUDIENCE", "maybe")
	if _, err := Load(); err == nil {
		t.Error("a non-boolean BOOTH_OIDC_REQUIRE_AUDIENCE was accepted")
	}
	t.Setenv("BOOTH_OIDC_REQUIRE_AUDIENCE", "true")
	if cfg, err := Load(); err != nil || !cfg.OIDC.RequireAudience || cfg.OIDC.ClientID != "booth" {
		t.Errorf("cfg = %+v, %v", cfg, err)
	}
}

func TestLoad_WorkloadIssuer(t *testing.T) {
	base(t)
	t.Setenv("BOOTH_WORKLOAD_ISSUER_URL", "http://booth-core.booth-system.svc:8080")
	if _, err := Load(); err == nil {
		t.Error("a workload issuer without an OIDC issuer was accepted")
	}
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp.example")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "booth")
	if cfg, err := Load(); err != nil || cfg.OIDC.WorkloadIssuerURL != "http://booth-core.booth-system.svc:8080" {
		t.Errorf("cfg = %+v, %v", cfg.OIDC, err)
	}
}

const runsOK = `{"image":"spark@sha256:x","imagePullPolicy":"IfNotPresent","maxRunning":2,"maxRunningPerWorkspace":1,
"memoryBudget":"4Gi","maxExecutors":2,"defaultExecutors":2,"maxMemory":"2g","maxDuration":"6h","pendingTimeout":"10m",
"driver":{"memory":"512m","cpu":{"request":"250m","limit":"1"}},"executor":{"memory":"512m","cpu":{"request":"250m","limit":"1"}},
"egress":{"mode":"open","exceptCidrs":["10.0.0.0/8"],"dns":{"namespaceSelector":{"a":"b"},"podSelector":{"c":"d"}}}}`

func runsEnv(t *testing.T) {
	t.Helper()
	base(t)
	t.Setenv("BOOTH_INSTANCE", "ns.booth-spark")
	t.Setenv("BOOTH_NAMESPACE", "ns")
	t.Setenv("BOOTH_SERVICE_ACCOUNT", "booth-spark")
	t.Setenv("BOOTH_DRIVER_CLUSTERROLE", "booth-spark-driver")
	t.Setenv("BOOTH_RUN_CONTROLLER_CLUSTERROLE", "booth-spark-run-controller")
	t.Setenv("BOOTH_BACKEND_POD_LABELS", `{"app":"booth-spark"}`)
}

func TestLoad_Runs(t *testing.T) {
	runsEnv(t)
	if cfg, err := Load(); err != nil || cfg.Runs != nil {
		t.Fatalf("unset: %+v %v", cfg.Runs, err)
	}
	t.Setenv("BOOTH_RUNS", runsOK)
	cfg, err := Load()
	if err != nil || cfg.Runs.MemoryBudgetMi != 4096 || cfg.Runs.PendingTimeoutD.String() != "10m0s" {
		t.Fatalf("ok: %+v %v", cfg.Runs, err)
	}
	for name, mut := range map[string]func(string) string{
		"unknown field":   func(s string) string { return strings.Replace(s, `"maxRunning":2`, `"maxRunning":2,"maxRuning":3`, 1) },
		"no image":        func(s string) string { return strings.Replace(s, `"spark@sha256:x"`, `""`, 1) },
		"bad pull policy": func(s string) string { return strings.Replace(s, `"IfNotPresent"`, `"Sometimes"`, 1) },
		"bad budget":      func(s string) string { return strings.Replace(s, `"4Gi"`, `"lots"`, 1) },
		"bad egress mode": func(s string) string { return strings.Replace(s, `"mode":"open"`, `"mode":"internet"`, 1) },
		"bad cidr":        func(s string) string { return strings.Replace(s, `"10.0.0.0/8"`, `"10.0.0.0/33"`, 1) },
		"no dns":          func(s string) string { return strings.Replace(s, `"podSelector":{"c":"d"}`, `"podSelector":{}`, 1) },
		"bad memory":      func(s string) string { return strings.Replace(s, `"maxMemory":"2g"`, `"maxMemory":"2Gi"`, 1) },
		"defaults > max":  func(s string) string { return strings.Replace(s, `"defaultExecutors":2`, `"defaultExecutors":3`, 1) },
	} {
		t.Setenv("BOOTH_RUNS", mut(runsOK))
		if _, err := Load(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	t.Setenv("BOOTH_RUNS", runsOK)
	t.Setenv("BOOTH_BACKEND_POD_LABELS", `{}`)
	if _, err := Load(); err == nil {
		t.Error("no backend pod labels: accepted")
	}
}

// ADR 0110: the submit floor is editor or owner, never viewer.
func TestLoad_SubmitMinRole(t *testing.T) {
	base(t)
	for v, ok := range map[string]bool{"editor": true, "owner": true, "viewer": false, "admin": false} {
		t.Setenv("BOOTH_SUBMIT_MIN_ROLE", v)
		cfg, err := Load()
		if ok != (err == nil) {
			t.Errorf("%s: err = %v", v, err)
		}
		if ok && string(cfg.SubmitMinRole) != v {
			t.Errorf("%s: got %q", v, cfg.SubmitMinRole)
		}
	}
}
