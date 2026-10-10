package config

import "testing"

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

func TestLoad_UIProofRuns(t *testing.T) {
	base(t)
	if cfg, err := Load(); err != nil || len(cfg.UIProofRuns) != 0 {
		t.Fatalf("default: %+v %v", cfg.UIProofRuns, err)
	}
	t.Setenv("BOOTH_UI_PROOF_RUNS", `[{"id":"proof-1","workspace":"acme","submitter":"u-1","url":"http://proof-driver.p.svc:4040"}]`)
	cfg, err := Load()
	if err != nil || len(cfg.UIProofRuns) != 1 || cfg.UIProofRuns[0].Submitter != "u-1" {
		t.Fatalf("set: %+v %v", cfg.UIProofRuns, err)
	}
	for name, v := range map[string]string{
		"not JSON":        `nope`,
		"unknown field":   `[{"id":"a","workspace":"w","submitter":"s","url":"http://x","owner":"o"}]`,
		"bad id":          `[{"id":"Proof","workspace":"w","submitter":"s","url":"http://x"}]`,
		"reserved id":     `[{"id":"proxy","workspace":"w","submitter":"s","url":"http://x"}]`,
		"duplicate":       `[{"id":"a","workspace":"w","submitter":"s","url":"http://x"},{"id":"a","workspace":"w","submitter":"s","url":"http://x"}]`,
		"no submitter":    `[{"id":"a","workspace":"w","url":"http://x"}]`,
		"not an http url": `[{"id":"a","workspace":"w","submitter":"s","url":"file:///etc"}]`,
	} {
		t.Setenv("BOOTH_UI_PROOF_RUNS", v)
		if _, err := Load(); err == nil {
			t.Errorf("%s: accepted", name)
		}
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
