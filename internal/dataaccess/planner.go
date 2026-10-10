package dataaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projectbooth/booth-spark/internal/runs"
)

// Planner is runs.Data: it resolves a run's data access before the run's launch, as the run itself
// (its own first token), and keeps the token current while the run lives.
type Planner struct {
	Tokens *Tokens
	// CoreURL is booth-core's in-cluster base: the broker (/api/credentials) and the gateway
	// (/modules/<id>/...).
	CoreURL string
	HTTP    *http.Client
}

func runOf(r runs.Run) Run { return Run{ID: r.ID, Workspace: r.Workspace, Submitter: r.Submitter} }

func (p *Planner) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Prepare implements runs.Data.
func (p *Planner) Prepare(ctx context.Context, r runs.Run) (runs.DataPlan, error) {
	tok, err := p.Tokens.Get(ctx, runOf(r))
	if Refused(err) {
		return runs.DataPlan{}, runs.DataRefusal("%v", err)
	}
	if err != nil {
		return runs.DataPlan{}, fmt.Errorf("minting the run's token: %w", err)
	}
	plan := runs.DataPlan{Role: tok.Role}
	d := r.Spec.DataAccess
	if d == nil {
		d = &runs.DataAccess{}
	}
	if d.Database {
		plan.Database = runs.WorkspaceDatabase(r.Workspace)
	}
	if d.Lakehouse {
		wh, err := p.warehouse(ctx, tok, r.Workspace)
		if err != nil {
			return runs.DataPlan{}, err
		}
		loc, err := p.resolve(ctx, tok, r.Workspace, wh.BackendID, wh.Path, plan.Access())
		if err != nil {
			return runs.DataPlan{}, fmt.Errorf("the workspace's warehouse: %w", err)
		}
		bucket, _, _ := strings.Cut(strings.TrimPrefix(wh.StorageRoot, "s3://"), "/")
		if !strings.HasPrefix(wh.StorageRoot, "s3://") || bucket != loc.Bucket {
			return runs.DataPlan{}, runs.DataRefusal("the workspace's warehouse is at %q, which isn't the bucket its storage lease names (%s)", wh.StorageRoot, loc.Bucket)
		}
		loc.Root = strings.TrimSuffix(wh.StorageRoot, "/")
		plan.Warehouse = &loc
	}
	for _, s := range d.Storage {
		if s.Access == "readwrite" && tok.Role != "editor" {
			return runs.DataPlan{}, runs.DataRefusal("storage location %s/%s asks for readwrite, which needs an editor; the run's submitter is now a %s in this workspace", s.BackendID, s.Path, tok.Role)
		}
		loc, err := p.resolve(ctx, tok, r.Workspace, s.BackendID, s.Path, s.Access)
		if err != nil {
			return runs.DataPlan{}, fmt.Errorf("storage location %s/%s: %w", s.BackendID, s.Path, err)
		}
		loc.Root = runs.StorageRoot(loc.Bucket, loc.KeyPrefix)
		plan.Storage = append(plan.Storage, loc)
	}
	if err := runs.CheckPlan(plan); err != nil {
		return runs.DataPlan{}, err
	}
	return plan, nil
}

// Check implements runs.Data. A run that started as an editor's ends when its submitter is no longer
// one: its sidecars hold read-write leases, which a viewer must not keep using until they expire.
func (p *Planner) Check(ctx context.Context, r runs.Run) error {
	tok, err := p.Tokens.Get(ctx, runOf(r))
	if Refused(err) {
		return runs.DataRefusal("%v", err)
	}
	if err != nil {
		return err
	}
	if r.Data != nil && r.Data.Role == "editor" && tok.Role != "editor" {
		return runs.DataRefusal("the run's submitter is now a %s in this workspace, and the run started with an editor's read and write access", tok.Role)
	}
	return nil
}

// Forget implements runs.Data.
func (p *Planner) Forget(id string) { p.Tokens.Forget(id) }

// warehouse is booth-lakehouse's GET /api/warehouse through core's gateway, as the run.
type warehouse struct {
	BackendID   string `json:"backendId"`
	Path        string `json:"path"`
	StorageRoot string `json:"storageRoot"`
}

func (p *Planner) warehouse(ctx context.Context, tok Token, ws string) (warehouse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.CoreURL, "/")+"/modules/lakehouse/api/warehouse", nil)
	if err != nil {
		return warehouse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.JWT)
	req.Header.Set("X-Workspace", ws)
	resp, err := p.client().Do(req)
	if err != nil {
		return warehouse{}, fmt.Errorf("booth-lakehouse: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return warehouse{}, runs.DataRefusal("the workspace has no lakehouse warehouse (or booth-lakehouse isn't installed); a workspace owner creates it once")
	case http.StatusUnauthorized, http.StatusForbidden:
		return warehouse{}, runs.DataRefusal("booth-lakehouse refused the run: %s", short(raw))
	default:
		return warehouse{}, fmt.Errorf("booth-lakehouse answered %d: %s", resp.StatusCode, short(raw))
	}
	var wh warehouse
	if err := json.Unmarshal(raw, &wh); err != nil || wh.BackendID == "" || wh.StorageRoot == "" {
		return warehouse{}, fmt.Errorf("booth-lakehouse: unreadable warehouse")
	}
	return wh, nil
}

// resolve asks the broker, as the run, for an s3 lease on {backendId, path} and keeps only where it
// is: the object store's endpoint, bucket and key prefix. The keys in the answer are dropped
// unread; the run's own s3 sidecars hold the leases its code uses. Asking first means a location the
// submitter can't have fails the run at once, with the broker's reason, before any pod exists.
func (p *Planner) resolve(ctx context.Context, tok Token, ws, backendID, path, access string) (runs.Location, error) {
	body, _ := json.Marshal(map[string]any{
		"kind": "s3", "access": access,
		"scope":   map[string]string{"backendId": backendID, "path": path},
		"options": map[string]string{"sessionToken": "allowed"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.CoreURL, "/")+"/api/credentials", bytes.NewReader(body))
	if err != nil {
		return runs.Location{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.JWT)
	req.Header.Set("X-Workspace", ws)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return runs.Location{}, fmt.Errorf("the credential broker: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusBadRequest:
		return runs.Location{}, runs.DataRefusal("the credential broker refused %s access to %s/%s: %s", access, backendID, path, brokerReason(raw))
	default:
		return runs.Location{}, fmt.Errorf("the credential broker answered %d", resp.StatusCode)
	}
	var out struct {
		Credential struct {
			Endpoint  string `json:"endpoint"`
			Region    string `json:"region"`
			Bucket    string `json:"bucket"`
			KeyPrefix string `json:"keyPrefix"`
			PathStyle bool   `json:"pathStyle"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Credential.Bucket == "" {
		return runs.Location{}, fmt.Errorf("the credential broker's answer names no bucket")
	}
	c := out.Credential
	return runs.Location{BackendID: backendID, Path: path, Access: access, Bucket: c.Bucket, KeyPrefix: c.KeyPrefix,
		Endpoint: c.Endpoint, Region: c.Region, PathStyle: c.PathStyle}, nil
}

// brokerReason is the broker's own message, never the body as such (it could carry a credential
// on an unexpected status).
func brokerReason(raw []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return "no reason given"
	}
	if e.Message != "" {
		return short([]byte(e.Message))
	}
	return short([]byte(e.Error))
}

func short(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ObjectURL is a booth-storage object's path through core's gateway.
func ObjectURL(backendID, path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/modules/storage/api/backends/" + url.PathEscape(backendID) + "/objects/" + strings.Join(segs, "/")
}
