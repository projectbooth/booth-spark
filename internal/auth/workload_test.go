package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/auth/authtest"
)

// booth-storage's workload-issuer tests (ADR 0056/0059), against authtest.NewCore: booth-core's
// workload issuer serves a JWKS at /.well-known/jwks.json and no discovery document.
func TestVerifier_WorkloadIssuer(t *testing.T) {
	idp := authtest.New(t)
	core := authtest.NewCore(t)
	ctx := context.Background()

	both, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-spark", RequireAudience: true, WorkloadIssuerURL: core.URL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	idpOnly, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-spark", RequireAudience: true})
	if err != nil {
		t.Fatal(err)
	}
	groups := []string{"/workspaces/acme/editor"}

	cases := []struct {
		name         string
		verifier     *auth.Verifier
		token        string
		wantSub      string
		wantWorkload bool
		wantErr      bool
	}{
		{"human token still verifies", both, idp.Mint(t, authtest.Token{Subject: "alice", Audience: "booth-spark", Groups: groups}), "alice", false, false},
		{"workload token verifies", both, core.Mint(t, authtest.Token{Subject: "pipeline:42", Audience: "booth-spark", Groups: groups}), "pipeline:42", true, false},
		{"workload token with no second issuer configured is refused", idpOnly, core.Mint(t, authtest.Token{Subject: "pipeline:42", Audience: "booth-spark", Groups: groups}), "", false, true},
		{"workload token audience is enforced like a human's", both, core.Mint(t, authtest.Token{Subject: "pipeline:42", Audience: "someone-else", Groups: groups}), "", false, true},
		{"expired workload token", both, core.Mint(t, authtest.Token{Subject: "pipeline:42", Audience: "booth-spark", Expiry: -time.Hour}), "", false, true},
		// Core never mints a person-shaped workload subject (ADR 0058); one that claims to come from
		// core is refused rather than treated as a person.
		{"workload issuer with a person-shaped subject", both, core.Mint(t, authtest.Token{Subject: "alice", Audience: "booth-spark", Groups: groups}), "", false, true},
		// Each issuer's keys are good for that issuer only: neither can vouch for the other.
		{"core's key claiming the IdP's issuer", both, core.Mint(t, authtest.Token{Subject: "mallory", Issuer: idp.URL, Audience: "booth-spark"}), "", false, true},
		{"IdP's key claiming core's issuer", both, idp.Mint(t, authtest.Token{Subject: "job:1", Issuer: core.URL, Audience: "booth-spark"}), "", false, true},
		{"an untrusted issuer", both, core.Mint(t, authtest.Token{Subject: "mallory", Issuer: "https://evil.example", Audience: "booth-spark"}), "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := tc.verifier.Verify(ctx, tc.token)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Verify succeeded with claims %+v, want an error", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.Subject != tc.wantSub || claims.Workload != tc.wantWorkload {
				t.Errorf("claims = %+v, want subject %q workload %v", claims, tc.wantSub, tc.wantWorkload)
			}
			// The role-derivation input (ADR 0041) is the same whichever issuer signed it.
			if got := auth.RoleInWorkspace(claims.Groups, "acme"); got != auth.RoleEditor {
				t.Errorf("role in acme = %q, want editor", got)
			}
		})
	}
}

// Trusting core must not depend on core being reachable when this module starts: its keys are
// fetched on the first workload token, and human tokens work meanwhile.
func TestVerifier_WorkloadIssuerDownAtStartup(t *testing.T) {
	idp := authtest.New(t)
	ctx := context.Background()
	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-spark", WorkloadIssuerURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewVerifier must not need core to be up: %v", err)
	}
	if _, err := v.Verify(ctx, idp.Mint(t, authtest.Token{Subject: "alice"})); err != nil {
		t.Errorf("human token refused while core is down: %v", err)
	}
}

func TestVerifier_WorkloadIssuerTrailingSlash(t *testing.T) {
	idp := authtest.New(t)
	core := authtest.NewCore(t)
	ctx := context.Background()
	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-spark", WorkloadIssuerURL: core.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, core.Mint(t, authtest.Token{Subject: "job:1"})); err != nil {
		t.Errorf("workload token refused when the issuer is configured with a trailing slash: %v", err)
	}
}

func TestNewVerifier_WorkloadIssuerMustDifferFromIdP(t *testing.T) {
	idp := authtest.New(t)
	if _, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, WorkloadIssuerURL: idp.URL}); err == nil {
		t.Error("NewVerifier accepted the IdP as its own workload issuer")
	}
	if _, err := auth.NewLazy(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, WorkloadIssuerURL: idp.URL + "/"}); err == nil {
		t.Error("NewLazy accepted the IdP as its own workload issuer")
	}
}

// The workload issuer and the key override combine: human keys from the JWKS URL, workload keys
// from core, discovery never contacted.
func TestVerifier_JWKSOverrideWithWorkloadIssuer(t *testing.T) {
	idp := authtest.New(t)
	core := authtest.NewCore(t)
	const issuer = "https://booth.home.arpa.invalid/realms/booth"
	ctx := context.Background()
	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: issuer, ClientID: "booth-spark", JWKSURL: idp.URL + "/jwks", WorkloadIssuerURL: core.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, idp.Mint(t, authtest.Token{Issuer: issuer, Subject: "alice"})); err != nil {
		t.Errorf("human token: %v", err)
	}
	if c, err := v.Verify(ctx, core.Mint(t, authtest.Token{Subject: "pipeline:7"})); err != nil || !c.Workload {
		t.Errorf("workload token: %+v %v", c, err)
	}
	if idp.DiscoveryHits.Load() != 0 || core.DiscoveryHits.Load() != 0 {
		t.Error("discovery was contacted")
	}
}

// Lazy lets the module start before the identity provider: until discovery works every call is
// ErrProviderUnavailable (503 from Middleware, not 401), and the first success is kept.
func TestLazy(t *testing.T) {
	idp := authtest.New(t)
	ctx := context.Background()
	down, err := auth.NewLazy(ctx, auth.OIDCConfig{IssuerURL: "http://127.0.0.1:1", ClientID: "booth-spark"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down.Verify(ctx, idp.Mint(t, authtest.Token{Subject: "alice"})); !errors.Is(err, auth.ErrProviderUnavailable) {
		t.Errorf("unreachable provider: err = %v, want ErrProviderUnavailable", err)
	}

	up, err := auth.NewLazy(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-spark"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := up.Verify(ctx, idp.Mint(t, authtest.Token{Subject: "alice"})); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	if n := idp.DiscoveryHits.Load(); n != 1 {
		t.Errorf("discovery requested %d times, want once (the verifier is kept)", n)
	}
	if _, err := auth.NewLazy(ctx, auth.OIDCConfig{JWKSURL: idp.URL + "/jwks"}); err == nil {
		t.Error("NewLazy accepted a JWKS URL without an issuer")
	}

	h := auth.Middleware(down)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") }))
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+idp.Mint(t, authtest.Token{Subject: "alice"}))
	req.Header.Set(auth.HeaderBoothWorkspace, "acme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503 while the provider is unreachable", rec.Code)
	}
}

// ADR 0094: the operator claim is read off the verified token, independently of the workspace
// role, and never from a workload token.
func TestMiddleware_OperatorAndWorkload(t *testing.T) {
	idp := authtest.New(t)
	core := authtest.NewCore(t)
	v, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-spark", WorkloadIssuerURL: core.URL})
	if err != nil {
		t.Fatal(err)
	}
	var got auth.Identity
	h := auth.Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = auth.FromContext(r.Context())
	}))
	cases := []struct {
		name               string
		token              string
		operator, workload bool
		role               auth.Role
	}{
		{"operator who is a viewer", idp.Mint(t, authtest.Token{Subject: "op", Groups: []string{"/workspaces/acme/viewer", "/platform/operator"}}), true, false, auth.RoleViewer},
		{"near-miss operator group", idp.Mint(t, authtest.Token{Subject: "op", Groups: []string{"/workspaces/acme/viewer", "/platform/operators"}}), false, false, auth.RoleViewer},
		{"workload token", core.Mint(t, authtest.Token{Subject: "pipeline:1", Groups: []string{"/workspaces/acme/editor", "/platform/operator"}}), false, true, auth.RoleEditor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got = auth.Identity{}
			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set(auth.HeaderBoothWorkspace, "acme")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if got.Operator != tc.operator || got.Workload != tc.workload || got.Role != tc.role {
				t.Errorf("identity = %+v", got)
			}
		})
	}
}
