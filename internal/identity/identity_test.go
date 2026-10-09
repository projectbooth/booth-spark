package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const issuer = "http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity"

// signer mints assertions shaped exactly like booth-core's iframeidentity.Service.Mint.
type signer struct {
	key *rsa.PrivateKey
	jws jose.Signer
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	return &signer{key: key, jws: s}
}

type mint struct {
	iss, sub, aud string
	groups        []string
	exp           time.Duration
}

func (s *signer) sign(t *testing.T, m mint) string {
	t.Helper()
	if m.iss == "" {
		m.iss = issuer
	}
	if m.aud == "" {
		m.aud = ModuleID
	}
	if m.exp == 0 {
		m.exp = 2 * time.Minute
	}
	now := time.Now()
	raw, err := jwt.Signed(s.jws).Claims(jwt.Claims{
		Issuer: m.iss, Subject: m.sub, Audience: jwt.Audience{m.aud},
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(m.exp)),
	}).Claims(map[string]any{"groups": m.groups, "booth_module": m.aud}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (s *signer) verifier() *Verifier {
	return NewWithKeySet(Config{IssuerURL: issuer}, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&s.key.PublicKey}})
}

func req(assertion string, headers ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ui/api/me", nil)
	if assertion != "" {
		r.Header.Set(HeaderIdentity, assertion)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

func TestVerify_ValidAssertion(t *testing.T) {
	s := newSigner(t)
	tok := s.sign(t, mint{sub: "3f1c-uuid", groups: []string{"/workspaces/acme/editor", "/platform/operator"}})
	c, err := s.verifier().Verify(req(tok, HeaderWorkspace, "acme", HeaderRole, "editor"))
	if err != nil {
		t.Fatal(err)
	}
	if c != (Caller{Subject: "3f1c-uuid", Workspace: "acme", Role: RoleEditor, Operator: true, DisplayName: "3f1c-uuid"}) {
		t.Errorf("caller = %+v", c)
	}
	// Without core's convenience headers the single membership decides.
	if c, err = s.verifier().Verify(req(tok)); err != nil || c.Workspace != "acme" {
		t.Errorf("without headers: %+v, %v", c, err)
	}
}

func TestVerify_Refusals(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)
	good := []string{"/workspaces/acme/viewer"}
	cases := []struct {
		name    string
		r       *http.Request
		wantErr error
	}{
		{"no assertion", req(""), ErrMissing},
		{"garbage", req("not-a-jwt"), ErrInvalid},
		{"signed by another key", req(other.sign(t, mint{sub: "u", groups: good})), ErrInvalid},
		{"wrong issuer (e.g. the .svc spelling)", req(s.sign(t, mint{iss: "http://booth-core.booth-system.svc:8080/iframe-identity", sub: "u", groups: good})), ErrInvalid},
		{"another module's audience", req(s.sign(t, mint{aud: "streamlit", sub: "u", groups: good})), ErrInvalid},
		{"expired", req(s.sign(t, mint{sub: "u", groups: good, exp: -time.Minute})), ErrInvalid},
		{"workload-shaped subject", req(s.sign(t, mint{sub: "job:42", groups: good})), ErrInvalid},
		{"no workspace membership", req(s.sign(t, mint{sub: "u", groups: []string{"/platform/operator"}})), ErrForbidden},
		{"header names a workspace the assertion doesn't grant", req(s.sign(t, mint{sub: "u", groups: good}), HeaderWorkspace, "other"), ErrForbidden},
		// ADR 0041: a forwarded role stronger than the assertion's is refused.
		{"role header escalates", req(s.sign(t, mint{sub: "u", groups: good}), HeaderRole, "owner"), ErrForbidden},
		{"near-miss group", req(s.sign(t, mint{sub: "u", groups: []string{"/workspaces/acme/admin", "/workspaces/Acme/owner"}})), ErrForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.verifier().Verify(c.r)
			if !errors.Is(err, c.wantErr) {
				t.Errorf("err = %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestVerify_WeakerRoleHeaderIsFine(t *testing.T) {
	s := newSigner(t)
	c, err := s.verifier().Verify(req(s.sign(t, mint{sub: "u", groups: []string{"/workspaces/acme/owner"}}), HeaderRole, "viewer"))
	if err != nil || c.Role != RoleOwner {
		t.Fatalf("caller = %+v, err = %v; the role comes from the assertion", c, err)
	}
}

// Discovery is lazy: constructing a Verifier against an unreachable core must not fail, so the
// module stays up while core is still starting.
func TestNew_DoesNotContactTheIssuer(t *testing.T) {
	if _, err := New(context.Background(), Config{IssuerURL: "http://127.0.0.1:1/iframe-identity"}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("an empty issuer must be refused")
	}
}

// ADR 0094 and core PR #3: the operator claim and preferred_username come from the assertion. The
// operator claim needs an exact match and is independent of the workspace role.
func TestVerify_OperatorAndDisplayName(t *testing.T) {
	s := newSigner(t)
	cases := []struct {
		name     string
		groups   []string
		extra    map[string]any
		operator bool
		display  string
	}{
		{"operator viewer, with a username", []string{"/workspaces/acme/viewer", "/platform/operator"}, map[string]any{"preferred_username": "olive"}, true, "olive"},
		{"no operator claim", []string{"/workspaces/acme/owner"}, nil, false, "u-1"},
		{"near-miss operator claim", []string{"/workspaces/acme/owner", "/platform/operator/"}, nil, false, "u-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.verifier().Verify(req(s.signExtra(t, mint{sub: "u-1", groups: c.groups}, c.extra)))
			if err != nil {
				t.Fatal(err)
			}
			if got.Operator != c.operator || got.DisplayName != c.display {
				t.Errorf("caller = %+v", got)
			}
		})
	}
}

func (s *signer) signExtra(t *testing.T, m mint, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	b := jwt.Signed(s.jws).Claims(jwt.Claims{
		Issuer: issuer, Subject: m.sub, Audience: jwt.Audience{ModuleID},
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(2 * time.Minute)),
	}).Claims(map[string]any{"groups": m.groups, "booth_module": ModuleID})
	if extra != nil {
		b = b.Claims(extra)
	}
	raw, err := b.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
