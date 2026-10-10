// Package dataaccess is how a run reads and writes data as its submitter (docs/design-v0.md
// item 4, ADR 0110): the backend mints one workload token per run from booth-core (subject
// spark:<workspace>:<runId>, owner the submitter, roleCeiling editor), keeps it in memory, and
// hands it only to that run's agents, over the backend's internal port, against the run's data
// bearer. The agents write it into a memory-backed volume in each pod, shared with the credential
// sidecars and the Spark container (whose code can read it: item 4's credential table).
//
// Core enforces the owner (its internal/workload/service.go): it refuses unless the submitter
// currently holds a role in the run's workspace and was seen within BOOTH_WORKLOAD_OWNER_MAX_AGE
// (7 days by default), and grants the lesser of roleCeiling and the submitter's role. A copy of
// booth-streamlit's internal/dataaccess minting, with the editor ceiling.
package dataaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RoleCeiling caps every run's token (ADR 0110 item 4: data access never needs owner).
const RoleCeiling = "editor"

// Subject names the run in core's audit log: spark:<workspace>:<runId>. Core's subject rule,
// ^[a-z][a-z0-9-]{0,31}:[A-Za-z0-9._:-]{1,200}$, admits it.
func Subject(workspace, runID string) string { return "spark:" + workspace + ":" + runID }

// Token is a minted workload token.
type Token struct {
	JWT       string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Role      string    `json:"role"`
}

// Access is the credential access a token's role allows: readwrite for an editor, read for a
// viewer (a submitter demoted since submitting).
func (t Token) Access() string {
	if t.Role == "editor" {
		return "readwrite"
	}
	return "read"
}

// Errors.
var (
	// ErrOwnerNoAccess: core refused the submitter (no role in the workspace any more, or not
	// seen for 7 days).
	ErrOwnerNoAccess = errors.New("the run's submitter no longer has access to this workspace, or hasn't signed in for 7 days")
	// ErrNotEntitled: core refused this module itself (manifest or minting credential); an
	// operator problem, not the submitter's.
	ErrNotEntitled = errors.New("booth-core refused to mint for booth-spark: check the manifest's workloadIdentity (dataAccess.enabled) and the booth-workload-minting-credentials Secret")
	// ErrRoleExceeded: core granted something other than editor or viewer. It never should (it
	// grants the lesser of the ceiling and the submitter's role); the token is discarded.
	ErrRoleExceeded = errors.New("booth-core granted a role a run may not use; refusing the token")
)

// Refused reports whether err means the run must lose its data access (and so end).
func Refused(err error) bool {
	return errors.Is(err, ErrOwnerNoAccess) || errors.Is(err, ErrRoleExceeded) || errors.Is(err, ErrNotEntitled)
}

// Minter mints workload tokens.
type Minter interface {
	Mint(ctx context.Context, workspace, subject, owner string) (Token, error)
}

// CoreMinter calls core's minting endpoint with the credential core delivers as the
// booth-workload-minting-credentials Secret (keys `url`, `credential`).
type CoreMinter struct {
	URL        string
	Credential string
	HTTP       *http.Client
}

// Mint implements Minter.
func (m *CoreMinter) Mint(ctx context.Context, workspace, subject, owner string) (Token, error) {
	body, _ := json.Marshal(map[string]string{"workspace": workspace, "subject": subject, "roleCeiling": RoleCeiling, "owner": owner})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Credential)
	req.Header.Set("Content-Type", "application/json")
	hc := m.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("minting: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
	case http.StatusForbidden:
		// Core answers 403 both for "the owner has no current access" and for "this module isn't
		// entitled"; its message says which (booth-core internal/api/workload.go; booth-streamlit
		// and booth-api read it the same way).
		if strings.Contains(strings.ToLower(string(raw)), "owner") {
			return Token{}, ErrOwnerNoAccess
		}
		return Token{}, ErrNotEntitled
	case http.StatusUnauthorized:
		return Token{}, ErrNotEntitled
	default:
		return Token{}, fmt.Errorf("minting: core answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out Token
	if err := json.Unmarshal(raw, &out); err != nil || out.JWT == "" {
		return Token{}, fmt.Errorf("minting: unreadable response from core")
	}
	return out, nil
}
