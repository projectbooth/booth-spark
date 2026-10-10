// Package config loads booth-spark's runtime configuration from environment variables. Every value
// maps 1:1 to a Helm chart value/env var, mirroring the sibling modules' own internal/config —
// there is no config file format of our own to version.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/uiproxy"
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

	// UIProofRuns is step 2's fixed run table (BOOTH_UI_PROOF_RUNS, JSON): runs whose Spark UI the
	// proxy may open, with their workspace, submitter and driver UI URL. It exists only to prove the
	// Spark UI path through a real core before the module can start drivers itself (step 3, which
	// replaces it with the module's run records). Empty in every real install; chart value
	// uiProof.runs.
	UIProofRuns []uiproxy.Run
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
	if v := os.Getenv("BOOTH_UI_PROOF_RUNS"); v != "" {
		dec := json.NewDecoder(strings.NewReader(v))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg.UIProofRuns); err != nil {
			return Config{}, fmt.Errorf("BOOTH_UI_PROOF_RUNS: %w", err)
		}
		seen := map[string]bool{}
		for _, r := range cfg.UIProofRuns {
			u, err := url.Parse(r.UIURL)
			switch {
			case !uiproxy.ValidID(r.ID):
				return Config{}, fmt.Errorf("BOOTH_UI_PROOF_RUNS: %q is not a valid run id", r.ID)
			case seen[r.ID]:
				return Config{}, fmt.Errorf("BOOTH_UI_PROOF_RUNS: run %q listed twice", r.ID)
			case r.Workspace == "" || r.Submitter == "":
				return Config{}, fmt.Errorf("BOOTH_UI_PROOF_RUNS: run %q needs a workspace and a submitter", r.ID)
			case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
				return Config{}, fmt.Errorf("BOOTH_UI_PROOF_RUNS: run %q has no http(s) url", r.ID)
			}
			seen[r.ID] = true
		}
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
