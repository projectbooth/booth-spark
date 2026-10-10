package dataaccess

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/runs"
)

// fakeCore is booth-core as booth-spark sees it: the minting endpoint, the broker, and the gateway
// to booth-lakehouse and booth-storage. It records what it was asked.
type fakeCore struct {
	mu       sync.Mutex
	mints    int
	brokered []string // "kind access scope X-Workspace"
	forwards []string // "METHOD path?query auth X-Workspace"
	srv      *httptest.Server
}

func jwtFor(sub string) string {
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	return enc(`{"alg":"none"}`) + "." + enc(`{"sub":"`+sub+`"}`) + ".sig"
}

func newFakeCore(t *testing.T) *fakeCore {
	f := &fakeCore{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/mint":
			f.mints++
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			if r.Header.Get("Authorization") != "Bearer mint-cred" || req["roleCeiling"] != "editor" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			role := map[string]string{"u-editor": "editor", "u-viewer": "viewer", "u-owner": "owner"}[req["owner"]]
			if role == "" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"the run's owner has no current access to that workspace"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": jwtFor(req["subject"]), "expiresAt": time.Now().Add(10 * time.Minute), "role": role})
		case r.URL.Path == "/api/credentials":
			var req struct {
				Kind, Access string
				Scope        map[string]string
			}
			_ = json.Unmarshal(body, &req)
			f.brokered = append(f.brokered, req.Kind+" "+req.Access+" "+req.Scope["backendId"]+"/"+req.Scope["path"]+" "+r.Header.Get("X-Workspace"))
			if req.Scope["backendId"] == "forbidden" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"forbidden","message":"no access to that backend"}`)
				return
			}
			bucket := "lake-bucket"
			if req.Scope["backendId"] == "other" {
				bucket = "other-bucket"
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"leaseId": "l1", "kind": "s3", "credential": map[string]any{
				"endpoint": "http://minio.minio.svc:9000", "bucket": bucket, "keyPrefix": "base/" + req.Scope["path"], "pathStyle": true,
				"accessKeyId": "AKIASECRET", "secretAccessKey": "SECRETKEY"}})
		case r.URL.Path == "/modules/lakehouse/api/warehouse":
			if r.Header.Get("X-Workspace") == "nowh" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"backendId":"lake","path":"warehouse","storageRoot":"s3://lake-bucket/base/warehouse"}`)
		default:
			f.forwards = append(f.forwards, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Workspace"))
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "from core: "+r.URL.Path)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTokens(f *fakeCore) *Tokens {
	return &Tokens{Minter: &CoreMinter{URL: f.srv.URL + "/mint", Credential: "mint-cred"}}
}

func dataRun(sub, ws string, d *runs.DataAccess) runs.Run {
	return runs.Run{ID: "r1", Workspace: ws, Submitter: sub, State: runs.Running,
		Spec: runs.Validated{Spec: runs.Spec{DataAccess: d}}}
}

func TestTokens_RemintAndRefusal(t *testing.T) {
	f := newFakeCore(t)
	now := time.Now()
	tk := newTokens(f)
	tk.Now = func() time.Time { return now }
	r := Run{ID: "r1", Workspace: "acme", Submitter: "u-editor"}
	a, err := tk.Get(context.Background(), r)
	if err != nil || a.Role != "editor" || a.Access() != "readwrite" {
		t.Fatalf("first: %+v %v", a, err)
	}
	if b, _ := tk.Get(context.Background(), r); b.JWT != a.JWT || f.mints != 1 {
		t.Errorf("re-minted before two thirds of the token's life (%d mints)", f.mints)
	}
	now = now.Add(7 * time.Minute)
	if _, err := tk.Get(context.Background(), r); err != nil || f.mints != 2 {
		t.Errorf("not re-minted after two thirds: %v, %d mints", err, f.mints)
	}

	gone := Run{ID: "r2", Workspace: "acme", Submitter: "u-gone"}
	if _, err := tk.Get(context.Background(), gone); !errors.Is(err, ErrOwnerNoAccess) {
		t.Errorf("a submitter without access: %v", err)
	}
	n := f.mints
	if _, err := tk.Get(context.Background(), gone); !errors.Is(err, ErrOwnerNoAccess) || f.mints != n {
		t.Errorf("a refusal isn't remembered: %v, %d mints", err, f.mints-n)
	}
	if _, err := tk.Get(context.Background(), Run{ID: "r3", Workspace: "acme", Submitter: "u-owner"}); !errors.Is(err, ErrRoleExceeded) {
		t.Errorf("an owner role beyond the ceiling: %v", err)
	}
	viewer, _ := tk.Get(context.Background(), Run{ID: "r4", Workspace: "acme", Submitter: "u-viewer"})
	if viewer.Access() != "read" {
		t.Errorf("a viewer's access = %s", viewer.Access())
	}
}

func TestPlanner_Prepare(t *testing.T) {
	f := newFakeCore(t)
	p := &Planner{Tokens: newTokens(f), CoreURL: f.srv.URL}
	ctx := context.Background()
	all := &runs.DataAccess{Database: true, Lakehouse: true, Storage: []runs.StorageLocation{
		{BackendID: "lake", Path: "raw", Access: "read"}, {BackendID: "other", Path: "out", Access: "readwrite"}}}
	plan, err := p.Prepare(ctx, dataRun("u-editor", "acme", all))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Role != "editor" || plan.Database != runs.WorkspaceDatabase("acme") || plan.Warehouse == nil ||
		plan.Warehouse.Root != "s3://lake-bucket/base/warehouse" || plan.Warehouse.Access != "readwrite" ||
		len(plan.Storage) != 2 || plan.Storage[0].Root != "s3a://lake-bucket/base/raw" || plan.Storage[1].Root != "s3a://other-bucket/base/out" ||
		plan.Storage[0].Endpoint != "http://minio.minio.svc:9000" || !plan.Storage[0].PathStyle {
		t.Errorf("plan = %+v", plan)
	}
	raw, _ := json.Marshal(plan)
	if strings.Contains(string(raw), "AKIA") || strings.Contains(string(raw), "SECRETKEY") {
		t.Error("the plan keeps a key from the broker's answer")
	}
	// Asked as the run itself: its own token, its own workspace.
	for _, b := range f.brokered {
		if !strings.HasSuffix(b, " acme") {
			t.Errorf("brokered %q, not in the run's workspace", b)
		}
	}

	for name, c := range map[string]struct {
		r    runs.Run
		want string
	}{
		"a submitter without access": {dataRun("u-gone", "acme", all), "no longer has access"},
		"readwrite as a viewer":      {dataRun("u-viewer", "acme", &runs.DataAccess{Storage: []runs.StorageLocation{{BackendID: "lake", Path: "x", Access: "readwrite"}}}), "needs an editor"},
		"no warehouse":               {dataRun("u-editor", "nowh", &runs.DataAccess{Lakehouse: true}), "no lakehouse warehouse"},
		"a backend not theirs":       {dataRun("u-editor", "acme", &runs.DataAccess{Storage: []runs.StorageLocation{{BackendID: "forbidden", Path: "x", Access: "read"}}}), "no access to that backend"},
		"two locations in one bucket": {dataRun("u-editor", "acme", &runs.DataAccess{Storage: []runs.StorageLocation{
			{BackendID: "lake", Path: "a", Access: "read"}, {BackendID: "lake", Path: "b", Access: "read"}}}), "same bucket"},
	} {
		p.Tokens.Forget("r1")
		_, err := p.Prepare(ctx, c.r)
		if !errors.Is(err, runs.ErrDataRefused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal mentioning %q", name, err, c.want)
		}
	}
	// A viewer reads: the lakehouse lease is read only.
	p.Tokens.Forget("r1")
	vplan, err := p.Prepare(ctx, dataRun("u-viewer", "acme", &runs.DataAccess{Lakehouse: true}))
	if err != nil || vplan.Warehouse.Access != "read" || vplan.Role != "viewer" {
		t.Errorf("viewer plan = %+v %v", vplan, err)
	}

	// Check: a submitter who loses access while the run lives is a refusal (a run's submitter never
	// changes; the cache is per run, so this one starts clean).
	p.Tokens.Forget("r1")
	if err := p.Check(ctx, dataRun("u-gone", "acme", all)); !errors.Is(err, runs.ErrDataRefused) {
		t.Errorf("Check after the submitter lost access: %v", err)
	}
}

type fakeStore struct{ runs map[string]runs.Run }

func (s fakeStore) Get(_ context.Context, id string) (runs.Run, error) {
	if r, ok := s.runs[id]; ok {
		return r, nil
	}
	return runs.Run{}, runs.ErrNotFound
}

func (s fakeStore) ByDataBearer(_ context.Context, b string) (runs.Run, error) {
	for _, r := range s.runs {
		if r.DataBearer == b {
			return r, nil
		}
	}
	return runs.Run{}, runs.ErrNotFound
}

func TestInternal(t *testing.T) {
	f := newFakeCore(t)
	bearer := strings.Repeat("b", 64)
	live := runs.Run{ID: "r1", Workspace: "acme", Submitter: "u-editor", State: runs.Running, DataBearer: bearer,
		Data: &runs.DataPlan{Role: "editor"}, Spec: runs.Validated{Spec: runs.Spec{Main: runs.Main{Python: &runs.FileRef{BackendID: "lake", Path: "jobs/etl.py"}}}}}
	ended := live
	ended.ID, ended.State, ended.DataBearer = "r2", runs.Succeeded, strings.Repeat("e", 64)
	gone := live
	gone.ID, gone.Submitter, gone.DataBearer = "r3", "u-gone", strings.Repeat("g", 64)
	in := &Internal{Store: fakeStore{map[string]runs.Run{"r1": live, "r2": ended, "r3": gone}}, Tokens: newTokens(f), CoreURL: f.srv.URL}
	h := in.Router()
	call := func(method, path, auth, ws, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if ws != "" {
			req.Header.Set("X-Workspace", ws)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// The token, against the run's data bearer only, while the run lives.
	rec := call("POST", "/internal/token", bearer, "", "")
	var tok Token
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &tok) != nil || tok.JWT == "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	for name, c := range map[string]struct {
		auth string
		want int
	}{"no bearer": {"", 401}, "an unknown bearer": {strings.Repeat("x", 64), 401}, "an ended run's bearer": {ended.DataBearer, 401},
		"a submitter who lost access": {gone.DataBearer, 403}} {
		if rec := call("POST", "/internal/token", c.auth, "", ""); rec.Code != c.want {
			t.Errorf("token, %s: %d %s", name, rec.Code, rec.Body)
		}
	}

	// The broker: only a live run's token, in its own workspace, known kinds.
	jwt := tok.JWT
	if rec := call("POST", "/internal/broker/api/credentials", jwt, "acme", `{"kind":"s3","access":"readwrite","scope":{"backendId":"lake","path":"x"}}`); rec.Code != 201 {
		t.Errorf("broker: %d %s", rec.Code, rec.Body)
	}
	for name, c := range map[string]struct{ auth, ws, body string }{
		"another workspace":          {jwt, "other", `{"kind":"s3","access":"read","scope":{}}`},
		"not a spark token":          {jwtFor("streamlit:acme:a1"), "acme", `{"kind":"s3","access":"read","scope":{}}`},
		"an ended run":               {jwtFor("spark:acme:r2"), "acme", `{"kind":"s3","access":"read","scope":{}}`},
		"an unknown kind":            {jwt, "acme", `{"kind":"nats","access":"read","scope":{}}`},
		"postgres for another space": {jwt, "acme", `{"kind":"postgres","access":"read","scope":{"workspace":"other"}}`},
		"an unknown access":          {jwt, "acme", `{"kind":"s3","access":"admin","scope":{}}`},
	} {
		n := len(f.brokered)
		if rec := call("POST", "/internal/broker/api/credentials", c.auth, c.ws, c.body); rec.Code < 400 || len(f.brokered) != n {
			t.Errorf("broker, %s: %d, forwarded=%t", name, rec.Code, len(f.brokered) != n)
		}
	}

	// Iceberg's REST catalog, forwarded to booth-lakehouse as the run.
	rec = call("GET", "/internal/lakehouse/iceberg/v1/config?warehouse=acme", jwt, "acme", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/modules/lakehouse/iceberg/v1/config") {
		t.Errorf("lakehouse: %d %s", rec.Code, rec.Body)
	}
	if last := f.forwards[len(f.forwards)-1]; last != "GET /modules/lakehouse/iceberg/v1/config?warehouse=acme Bearer "+jwt+" acme" {
		t.Errorf("forwarded %q", last)
	}
	for _, p := range []string{"/internal/lakehouse/api/warehouse", "/internal/lakehouse/iceberg/../api/admin/warehouses"} {
		if rec := call("GET", p, jwt, "acme", ""); rec.Code != 404 {
			t.Errorf("lakehouse %s: %d", p, rec.Code)
		}
	}

	// The entry point, from booth-storage as the run.
	rec = call("GET", "/internal/main", bearer, "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/modules/storage/api/backends/lake/objects/jobs/etl.py") {
		t.Errorf("main: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/internal/main", ended.DataBearer, "", ""); rec.Code != 401 {
		t.Errorf("main for an ended run: %d", rec.Code)
	}
}
