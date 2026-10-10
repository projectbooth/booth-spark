// Package auth verifies callers of booth-spark's published job-submission API (/v1/*), reached
// through booth-core's gateway with a bearer token (docs/design-v0.md item 6). Two kinds of caller
// are accepted there and nowhere else: people, with a token from the deployment's OIDC provider,
// and unattended runs (a pipeline task), with a workload token from booth-core's own issuer
// (ADR 0056/0059). The iframe routes (the module's UI and the Spark UI proxy) never accept a
// bearer; they verify booth-core's X-Booth-Identity assertion instead (internal/identity).
//
// A copy of booth-api's internal/auth (itself booth-database's, itself booth-storage's, the fleet's
// reference implementation of ADR 0041), including its ADR 0108 key-fetch override, plus
// booth-storage's second, workload issuer. The bearer token is re-verified against the same
// provider booth-core uses, and the caller's role is re-derived from the token's own groups claim
// (ADR 0025). The forwarded X-Booth-Role header can only narrow that role, and a header claiming
// more than the token grants is rejected with 403.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
)

// Header names the gateway forwards to a backing module (ADR 0025).
const (
	HeaderBoothWorkspace = "X-Booth-Workspace"
	HeaderBoothRole      = "X-Booth-Role"
)

// OperatorGroup is the workspace-independent platform-operator claim (ADR 0094).
const OperatorGroup = "/platform/operator"

// Role is a caller's workspace role (ADR 0025).
type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Claims is the subset of a verified token this module cares about.
type Claims struct {
	Subject string
	// Groups is the token's workspace-membership claim (ADR 0025), e.g. "/workspaces/acme/owner".
	Groups []string
	// DisplayName is preferred_username, else email, else the subject.
	DisplayName string
	// Workload is true for a token from booth-core's workload issuer: an unattended run, never a
	// person (ADR 0056). A run it submits gets no data access in v0 (ADR 0110 ruling 5).
	Workload bool
}

// TokenVerifier verifies a raw bearer token. *Verifier and *Lazy are the production
// implementations; the interface exists so the HTTP layer can be tested without a live provider.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (*Claims, error)
}

// ErrProviderUnavailable means the identity provider could not be reached to set up verification
// (discovery failed). Middleware answers 503, not 401: the caller's token may well be fine.
var ErrProviderUnavailable = errors.New("identity provider unavailable")

// OIDCConfig is the identity-provider configuration — the same shape as booth-core's, so every
// module verifies against the same provider.
type OIDCConfig struct {
	IssuerURL       string
	ClientID        string
	RequireAudience bool
	// GroupsClaim names the claim carrying workspace memberships; must match booth-core's.
	// Empty means "groups".
	GroupsClaim string
	// JWKSURL, if set, is where signing keys for IssuerURL are fetched from instead of the
	// issuer's discovery document (ADR 0108). `iss` is still validated exactly against IssuerURL.
	// Empty means discovery, as before. The fetch is unauthenticated and, on the bundled install,
	// plain http in-cluster: it relies on NetworkPolicy and cluster trust.
	JWKSURL string
	// WorkloadIssuerURL, if set, is booth-core's own workload-token issuer (ADR 0056), trusted as a
	// second issuer. Its keys are read from <WorkloadIssuerURL>/.well-known/jwks.json, lazily, so
	// core need not be up when this module starts. Empty trusts the OIDC provider only.
	WorkloadIssuerURL string
}

// DefaultGroupsClaim matches booth-core's default (ADR 0025).
const DefaultGroupsClaim = "groups"

// workloadJWKSPath is where booth-core publishes its workload-token signing keys, relative to its
// issuer URL (ADR 0056).
const workloadJWKSPath = "/.well-known/jwks.json"

// Verifier verifies bearer tokens against booth-core's OIDC provider and, when configured, against
// booth-core's workload-token issuer.
type Verifier struct {
	// byIssuer maps a trusted issuer URL to the verifier for it. A token's (unverified) iss only
	// selects which verifier runs; that verifier then checks the issuer itself along with
	// signature, expiry and audience, so a forged iss gains nothing.
	byIssuer       map[string]*oidc.IDTokenVerifier
	workloadIssuer string
	groupsClaim    string
}

// NewVerifier prepares token verification. Ordinarily it runs OIDC discovery against the issuer
// and uses the provider's own jwks_uri. If cfg.JWKSURL is set (ADR 0108), discovery is skipped
// entirely and keys are fetched from that URL; `iss` is still validated exactly against
// cfg.IssuerURL. It logs the issuer and where keys come from once, on success; it never logs a
// token.
func NewVerifier(ctx context.Context, cfg OIDCConfig) (*Verifier, error) {
	if cfg.JWKSURL != "" && cfg.IssuerURL == "" {
		return nil, fmt.Errorf("oidc.jwksUrl is set but oidc.issuerUrl is empty: the issuer is still required to validate `iss`")
	}
	verifierCfg := &oidc.Config{SkipClientIDCheck: !cfg.RequireAudience, ClientID: cfg.ClientID}
	var idv *oidc.IDTokenVerifier
	keysFrom := "discovery (" + cfg.IssuerURL + "/.well-known/openid-configuration)"
	if cfg.JWKSURL != "" {
		idv = oidc.NewVerifier(cfg.IssuerURL, oidc.NewRemoteKeySet(ctx, cfg.JWKSURL), verifierCfg)
		keysFrom = cfg.JWKSURL
	} else {
		provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery against %s: %w", cfg.IssuerURL, err)
		}
		idv = provider.Verifier(verifierCfg)
	}
	claim := cfg.GroupsClaim
	if claim == "" {
		claim = DefaultGroupsClaim
	}
	v := &Verifier{byIssuer: map[string]*oidc.IDTokenVerifier{cfg.IssuerURL: idv}, groupsClaim: claim}
	// Core strips a trailing slash from its issuer URL, so match that form in `iss`.
	if wl := strings.TrimRight(cfg.WorkloadIssuerURL, "/"); wl != "" {
		if wl == strings.TrimRight(cfg.IssuerURL, "/") {
			return nil, fmt.Errorf("the workload issuer must differ from the OIDC issuer (both %s)", wl)
		}
		v.byIssuer[wl] = oidc.NewVerifier(wl, oidc.NewRemoteKeySet(ctx, wl+workloadJWKSPath), verifierCfg)
		v.workloadIssuer = wl
	}
	log.Printf("oidc: verifying tokens with issuer=%s keys-from=%s workload-issuer=%s", cfg.IssuerURL, keysFrom, orNone(v.workloadIssuer))
	return v, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// unverifiedIssuer reads the iss claim from a JWT's payload without checking anything. It is only
// ever used to choose which verifier to hand the token to.
func unverifiedIssuer(rawToken string) (string, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("malformed token payload: %w", err)
	}
	var p struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", fmt.Errorf("malformed token payload: %w", err)
	}
	return p.Issuer, nil
}

// Verify checks signature, issuer, expiry and (when configured) audience, then reads the groups
// claim. A token from any issuer not configured here — booth-core's iframe-identity issuer
// included — fails.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	issuer, err := unverifiedIssuer(rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}
	verifier, ok := v.byIssuer[issuer]
	if !ok {
		return nil, fmt.Errorf("token verification failed: issuer %q is not trusted", issuer)
	}
	idToken, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}
	if idToken.Subject == "" {
		return nil, fmt.Errorf("token missing subject")
	}
	var raw map[string]json.RawMessage
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("reading token claims: %w", err)
	}
	var groups []string
	if g, ok := raw[v.groupsClaim]; ok {
		// A claim of the wrong shape is "no groups" (fail closed), not an error.
		_ = json.Unmarshal(g, &groups)
	}
	workload := v.workloadIssuer != "" && issuer == v.workloadIssuer
	if workload && !workloadSubject.MatchString(idToken.Subject) {
		// Core enforces `<kind>:<id>` when minting (ADR 0058); anything else is not a token it made.
		return nil, fmt.Errorf("token verification failed: workload token subject %q is not <kind>:<id>", idToken.Subject)
	}
	display := idToken.Subject
	for _, name := range []string{"preferred_username", "email"} {
		var s string
		if json.Unmarshal(raw[name], &s) == nil && s != "" {
			display = s
			break
		}
	}
	return &Claims{Subject: idToken.Subject, Groups: groups, DisplayName: display, Workload: workload}, nil
}

// workloadSubject is ADR 0058's `<kind>:<id>` shape, which no person's `sub` ever has.
var workloadSubject = regexp.MustCompile(`^[a-z][a-z0-9-]*:.+$`)

// Lazy builds a Verifier on first use and keeps it, so this module starts (and stays healthy)
// while the identity provider is still coming up, as booth-core itself does. Until discovery
// succeeds every Verify returns ErrProviderUnavailable; a failed attempt is retried at most every
// retryAfter.
type Lazy struct {
	ctx        context.Context
	cfg        OIDCConfig
	retryAfter time.Duration

	mu      sync.Mutex
	v       *Verifier
	lastTry time.Time
	lastErr error
}

// NewLazy returns a Lazy verifier. Configuration errors that discovery can't fix (a key URL
// without an issuer, the workload issuer equal to the OIDC one) are reported now, not later.
func NewLazy(ctx context.Context, cfg OIDCConfig) (*Lazy, error) {
	if cfg.JWKSURL != "" && cfg.IssuerURL == "" {
		return nil, fmt.Errorf("oidc.jwksUrl is set but oidc.issuerUrl is empty: the issuer is still required to validate `iss`")
	}
	if wl := strings.TrimRight(cfg.WorkloadIssuerURL, "/"); wl != "" && wl == strings.TrimRight(cfg.IssuerURL, "/") {
		return nil, fmt.Errorf("the workload issuer must differ from the OIDC issuer (both %s)", wl)
	}
	return &Lazy{ctx: ctx, cfg: cfg, retryAfter: 5 * time.Second}, nil
}

// Verify verifies with the underlying Verifier, building it first if need be.
func (l *Lazy) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	l.mu.Lock()
	if l.v == nil && time.Since(l.lastTry) >= l.retryAfter {
		l.lastTry = time.Now()
		l.v, l.lastErr = NewVerifier(l.ctx, l.cfg)
		if l.lastErr != nil {
			log.Printf("oidc: %v (retrying on a later request)", l.lastErr)
		}
	}
	v := l.v
	l.mu.Unlock()
	if v == nil {
		return nil, ErrProviderUnavailable
	}
	return v.Verify(ctx, rawToken)
}

// groupRE is ADR 0025's workspace-membership group shape.
var groupRE = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)

// Rank orders roles; 0 means not a role.
func Rank(r Role) int {
	switch r {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// RoleInWorkspace returns the highest role the groups grant in workspace, or "" if none.
func RoleInWorkspace(groups []string, workspace string) Role {
	var best Role
	for _, g := range groups {
		m := groupRE.FindStringSubmatch(g)
		if m == nil || m[1] != workspace {
			continue
		}
		if r := Role(m[2]); Rank(r) > Rank(best) {
			best = r
		}
	}
	return best
}

// IsOperator reports whether the groups carry the exact /platform/operator claim (ADR 0094).
func IsOperator(groups []string) bool {
	for _, g := range groups {
		if g == OperatorGroup {
			return true
		}
	}
	return false
}

// EffectiveRole is never stronger than the token's grant, nor than the forwarded role (a gateway
// may narrow, never widen). An absent forwarded role means "use the token's"; an unrecognized one
// yields "" — no access.
func EffectiveRole(forwarded, granted Role) Role {
	if forwarded == "" {
		return granted
	}
	if Rank(forwarded) == 0 {
		return ""
	}
	if Rank(forwarded) < Rank(granted) {
		return forwarded
	}
	return granted
}

// Identity is the caller identity attached to a request's context by Middleware.
type Identity struct {
	Subject     string
	DisplayName string
	Workspace   string
	Role        Role
	// Operator is the ADR 0094 claim, read off the verified token, independent of Role. A workload
	// token never carries it.
	Operator bool
	// Workload is true for booth-core's workload tokens (an unattended run, ADR 0056).
	Workload bool
}

type contextKey struct{}

// FromContext returns the identity Middleware attached, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// WithIdentity attaches an identity the way Middleware does — for handler tests.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// Middleware verifies the bearer token, reads the gateway-forwarded workspace, and derives the
// caller's role there from the token itself (ADR 0041).
func Middleware(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				WriteError(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			claims, err := verifier.Verify(r.Context(), token)
			if errors.Is(err, ErrProviderUnavailable) {
				WriteError(w, http.StatusServiceUnavailable, "the identity provider is not reachable yet")
				return
			}
			if err != nil {
				WriteError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			workspace := r.Header.Get(HeaderBoothWorkspace)
			if workspace == "" {
				WriteError(w, http.StatusBadRequest, "missing "+HeaderBoothWorkspace+" header")
				return
			}
			granted := RoleInWorkspace(claims.Groups, workspace)
			if granted == "" {
				WriteError(w, http.StatusForbidden, "your token grants no role in this workspace")
				return
			}
			forwarded := Role(r.Header.Get(HeaderBoothRole))
			if Rank(forwarded) > Rank(granted) {
				log.Printf("auth: rejected: forwarded role %q exceeds token-derived role %q for sub=%s workspace=%s", forwarded, granted, claims.Subject, workspace)
				WriteError(w, http.StatusForbidden, "the forwarded role exceeds what your token grants in this workspace")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), Identity{
				Subject: claims.Subject, DisplayName: claims.DisplayName, Workspace: workspace,
				Role: EffectiveRole(forwarded, granted), Operator: !claims.Workload && IsOperator(claims.Groups),
				Workload: claims.Workload,
			})))
		})
	}
}

// WriteError writes the JSON error body used across this module's API.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
