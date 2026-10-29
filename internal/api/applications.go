package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
	"github.com/projectbooth/booth-spark/internal/uiproxy"
)

// RunStore is what the API needs from the run records; *runs.Store implements it.
type RunStore interface {
	Create(ctx context.Context, r runs.Run, idempotencyKey string, a runs.Admission) (runs.Run, error)
	Get(ctx context.Context, id string) (runs.Run, error)
	List(ctx context.Context, workspace, kind string, limit int) ([]runs.Run, error)
	RequestStop(ctx context.Context, id string) error
	LogTail(ctx context.Context, id string) (string, error)
	LogTailAt(ctx context.Context, id string) (string, int64, error)
	AddStatement(ctx context.Context, runID, kind, code string, maxOpen int) (runs.Statement, error)
	Statements(ctx context.Context, runID string) ([]runs.Statement, error)
	GetStatement(ctx context.Context, runID, id string) (runs.Statement, error)
	Usage(ctx context.Context, workspace string) (runs.Usage, error)
}

// Applications is the /v1 job-submission API's configuration (docs/design-v0.md item 6).
type Applications struct {
	Store     RunStore
	Limits    runs.Limits
	Admission runs.Admission
	// DriverCPU and ExecutorCPU are each pod's CPU request and limit (runs.driver.cpu,
	// runs.executor.cpu), shown by GET /v1/cluster.
	DriverCPU, ExecutorCPU runs.CPU
	// LiveLogs reads the last lines of a live run's driver log.
	LiveLogs func(ctx context.Context, ns string, tailLines int64) (string, error)
	// LiveLogFrom reads a live run's driver log from a byte offset, at most max bytes, and says how
	// far the log reached (less than from when it is shorter): the log cursor.
	LiveLogFrom func(ctx context.Context, ns string, from int64, max int) ([]byte, int64, error)
}

// runView is a run as /v1 shows it.
type runView struct {
	runs.Run
	Resources struct {
		DriverMemory   string `json:"driverMemory"`
		ExecutorMemory string `json:"executorMemory"`
		MinExecutors   int    `json:"minExecutors"`
		MaxExecutors   int    `json:"maxExecutors"`
		MaxDuration    string `json:"maxDuration"`
	} `json:"resources"`
	// UIPath is where the run's Spark UI opens in the shell while it runs (its submitter only;
	// never for a run a workload submitted).
	UIPath string `json:"uiPath,omitempty"`
	// DataAccess is what the run asked for and, once it launched, what it got.
	DataAccess *dataView `json:"dataAccess,omitempty"`
	// Main is the run's entry point without its code: the inline Python's length and SHA-256, or
	// the file in booth-storage.
	Main mainView `json:"main"`
	// Origin is what started the run.
	Origin originView `json:"origin"`
}

// originView is what started a run, from what booth-spark records at submit:
//   - "person": someone's own token, through this module's page or /v1;
//   - "pipeline": a pipeline task (ADR 0119): a workload token whose core-signed booth_module
//     named the pipeline module (SubmitterModule), with a pipeline:<workspace>:<run> subject. A run
//     recorded before the module was kept counts only if its data owner was set, which needed that
//     same claim; the subject's prefix alone proves nothing. PipelineRun is from its subject, and
//     Task and Attempt from the Idempotency-Key booth-pipeline sends, <run>/<taskKey>/<attempt>,
//     when that key starts with the same run;
//   - "workload": any other workload token. Module is its core-signed booth_module (empty for a
//     run recorded before it was kept), and Workload its subject's kind (the part before the first
//     colon), as the minting module chose it. The page names known modules from a table.
type originView struct {
	Kind        string `json:"kind"`
	Module      string `json:"module,omitempty"`
	Workload    string `json:"workload,omitempty"`
	PipelineRun string `json:"pipelineRun,omitempty"`
	Task        string `json:"task,omitempty"`
	Attempt     string `json:"attempt,omitempty"`
}

func origin(r runs.Run) originView {
	byPipeline := r.SubmitterModule == PipelineModule || r.SubmitterModule == "" && r.DataOwner != ""
	switch {
	case !r.Workload:
		return originView{Kind: "person"}
	case !byPipeline || auth.WorkloadKind(r.Submitter) != "pipeline":
		return originView{Kind: "workload", Module: r.SubmitterModule, Workload: auth.WorkloadKind(r.Submitter)}
	}
	o := originView{Kind: "pipeline", Module: PipelineModule}
	if parts := strings.SplitN(r.Submitter, ":", 3); len(parts) == 3 {
		o.PipelineRun = parts[2]
	}
	if rest, ok := strings.CutPrefix(r.IdempotencyKey, o.PipelineRun+"/"); ok && o.PipelineRun != "" {
		if i := strings.LastIndex(rest, "/"); i > 0 {
			o.Task, o.Attempt = rest[:i], rest[i+1:]
		}
	}
	return o
}

// mainView is an entry point as /v1 shows it. The inline code itself is never shown.
type mainView struct {
	InlinePythonBytes  int           `json:"inlinePythonBytes,omitempty"`
	InlinePythonSHA256 string        `json:"inlinePythonSha256,omitempty"`
	InlineScalaBytes   int           `json:"inlineScalaBytes,omitempty"`
	InlineScalaSHA256  string        `json:"inlineScalaSha256,omitempty"`
	Python             *runs.FileRef `json:"python,omitempty"`
	Scala              *runs.FileRef `json:"scala,omitempty"`
	Jar                *runs.FileRef `json:"jar,omitempty"`
	MainClass          string        `json:"mainClass,omitempty"`
}

// dataView is a run's data access as /v1 shows it: no credential, no object-store address.
type dataView struct {
	Database  bool                   `json:"database,omitempty"`
	Lakehouse bool                   `json:"lakehouse,omitempty"`
	Storage   []runs.StorageLocation `json:"storage,omitempty"`
	// Role is the run's token's role when it launched: editor (read and write) or viewer (read).
	Role string `json:"role,omitempty"`
	// WarehouseRoot and StorageRoots are where the run's code finds its data.
	WarehouseRoot string   `json:"warehouseRoot,omitempty"`
	StorageRoots  []string `json:"storageRoots,omitempty"`
}

func dview(r runs.Run) *dataView {
	d := r.Spec.DataAccess
	if !d.Any() {
		return nil
	}
	v := &dataView{Database: d.Database, Lakehouse: d.Lakehouse, Storage: d.Storage}
	if p := r.Data; p != nil {
		v.Role = p.Role
		if p.Warehouse != nil {
			v.WarehouseRoot = p.Warehouse.Root
		}
		for _, l := range p.Storage {
			v.StorageRoots = append(v.StorageRoots, l.Root)
		}
	}
	return v
}

func view(r runs.Run) runView {
	m := r.Spec.Main
	v := runView{Run: r, DataAccess: dview(r), Main: mainView{InlinePythonBytes: m.InlinePythonBytes, InlinePythonSHA256: m.InlinePythonSHA256,
		InlineScalaBytes: m.InlineScalaBytes, InlineScalaSHA256: m.InlineScalaSHA256, Python: m.Python, Scala: m.Scala, Jar: m.Jar, MainClass: m.MainClass},
		Origin: origin(r)}
	v.Resources.DriverMemory = strconv.Itoa(r.Spec.DriverHeapMi) + "m"
	v.Resources.ExecutorMemory = strconv.Itoa(r.Spec.ExecutorHeapMi) + "m"
	v.Resources.MinExecutors = r.Spec.MinExecutors
	v.Resources.MaxExecutors = r.Spec.MaxExecutors
	v.Resources.MaxDuration = r.Spec.Duration.String()
	// A run a workload submitted has no Spark UI link: it has no person submitter to be its author
	// (ADR 0119 decision 3).
	if r.State == runs.Running && !r.Workload {
		v.UIPath = uiproxy.BasePath(r.ID) + "/"
	}
	return v
}

// apiError writes /v1's error body: {error, code}.
func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func (a Applications) routes(r chi.Router, minRole auth.Role) {
	r.Post("/applications", a.submit(minRole))
	r.Get("/applications", a.list)
	r.Get("/applications/{id}", a.get)
	r.Post("/applications/{id}/stop", a.stop)
	r.Get("/applications/{id}/logs", a.logs)
	r.Get("/applications/{id}/spec", a.spec)
	r.Get("/cluster", a.cluster)
	a.sessionRoutes(r, minRole)
}

func (a Applications) submit(minRole auth.Role) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		if !canSubmit(id.Role, minRole) {
			apiError(w, http.StatusForbidden, "forbidden", "submitting runs needs the "+string(minRole)+" role or above in this workspace")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > 200 {
			apiError(w, http.StatusBadRequest, "invalid", "Idempotency-Key: at most 200 characters")
			return
		}
		var spec runs.Spec
		dec := json.NewDecoder(io.LimitReader(r.Body, 512<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			apiError(w, http.StatusBadRequest, "invalid", "body: "+err.Error())
			return
		}
		var dataOwner, dataCeiling string
		if id.Workload && (spec.DataAccess.Any() || spec.Main.File() != nil) {
			// An entry point in booth-storage is read with the run's token too, so it counts as
			// data access.
			owner, why := workloadDataOwner(id)
			if why != "" {
				apiError(w, http.StatusUnprocessableEntity, "unavailable", why)
				return
			}
			dataOwner, dataCeiling = owner, dataRoleCeiling(id.Role)
		}
		v, err := runs.Validate(spec, a.Limits)
		var verr runs.ValidationError
		var unav runs.Unavailable
		switch {
		case errors.As(err, &verr):
			apiError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		case errors.As(err, &unav):
			apiError(w, http.StatusUnprocessableEntity, "unavailable", err.Error())
			return
		case err != nil:
			log.Printf("api: validate: %v", err)
			apiError(w, http.StatusInternalServerError, "internal", "could not validate the run")
			return
		}
		runID := runs.NewID()
		run, err := a.Store.Create(r.Context(), runs.Run{
			ID: runID, Kind: "application", Workspace: id.Workspace, Submitter: id.Subject, SubmitterName: id.DisplayName,
			Workload: id.Workload, Name: spec.Name, Namespace: runs.NamespacePrefix + runID, FootprintMi: v.FootprintMi(), Spec: v,
			DataBearer: dataBearer(v), DataOwner: dataOwner, DataCeiling: dataCeiling, SubmitterModule: submitterModule(id),
		}, key, a.Admission)
		var capErr runs.ErrAtCapacity
		switch {
		case errors.Is(err, runs.ErrDuplicate):
			writeJSON(w, http.StatusOK, view(run))
			return
		case errors.As(err, &capErr):
			apiError(w, http.StatusTooManyRequests, "at_capacity", err.Error())
			return
		case err != nil:
			log.Printf("api: create run: %v", err)
			apiError(w, http.StatusInternalServerError, "internal", "could not record the run")
			return
		}
		if run.DataOwner != "" {
			log.Printf("api: run %s submitted by sub=%s workspace=%s workload=true footprint=%dMi, data as owner=%s ceiling=%s",
				run.ID, id.Subject, id.Workspace, run.FootprintMi, run.DataOwner, run.DataCeiling)
		} else {
			log.Printf("api: run %s submitted by sub=%s workspace=%s workload=%v footprint=%dMi", run.ID, id.Subject, id.Workspace, id.Workload, run.FootprintMi)
		}
		writeJSON(w, http.StatusCreated, view(run))
	}
}

// PipelineModule is booth-pipeline's module id, as core names it in booth_module (its BoothModule's
// spec.id).
const PipelineModule = "pipeline"

// workloadDataOwner is ADR 0119's rule for a run with data access submitted with a workload token
// (amendment 3, ruling 1): the token is booth-core's workload issuer's (id.Workload), core's signed
// booth_module claim names the pipeline module, and it carries core's signed booth_owner claim.
// The trust is the module claim, which core binds to the minting credential; the subject is only
// a second guard (pipeline:<ws>:<runId>), since any minting module may choose any subject. The
// run then reads and writes as the owner, an opaque id core resolves at every mint (a person's sub
// or a service account, ADR 0120). Never an owner from the request: the body has no such field,
// and an unknown field is refused. Any other workload token keeps ADR 0110 ruling 5: no data
// access; a run's own spark token, which its code can read, among them.
func workloadDataOwner(id auth.Identity) (owner, why string) {
	switch {
	case id.Module != PipelineModule:
		return "", "a run submitted with a workload token gets data access only when the pipeline module submits it (ADR 0119); this token was minted for " + orUnnamed(id.Module)
	case auth.WorkloadKind(id.Subject) != "pipeline":
		return "", "a run submitted with a workload token gets data access only under a pipeline run's subject, pipeline:<workspace>:<run> (ADR 0119); this token's subject is " + id.Subject
	case id.Owner == "":
		return "", "a run submitted with a pipeline's workload token gets data access only when the token names its owner (booth_owner, signed by booth-core; ADR 0119), and this one doesn't"
	}
	return id.Owner, ""
}

// submitterModule is what a run records as its submitting module: a workload token's core-signed
// booth_module (auth reads it off core's own tokens only), and nothing for a person.
func submitterModule(id auth.Identity) string {
	if !id.Workload {
		return ""
	}
	return id.Module
}

func orUnnamed(module string) string {
	if module == "" {
		return "no named module"
	}
	return "module " + module
}

// dataRoleCeiling caps a pipeline-submitted run's token at the lesser of editor and the submitting
// token's role, fixed at submit (ADR 0119 decision 2; amendment 2, ruling 4).
func dataRoleCeiling(role auth.Role) string {
	if auth.Rank(role) < auth.Rank(auth.RoleEditor) {
		return string(auth.RoleViewer)
	}
	return string(auth.RoleEditor)
}

func (a Applications) list(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	list, err := a.Store.List(r.Context(), id.Workspace, "application", listMax)
	if err != nil {
		log.Printf("api: list runs: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not list runs")
		return
	}
	mine := r.URL.Query().Get("mine") == "true"
	state := r.URL.Query().Get("state")
	limit := listMax
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > listMax {
			apiError(w, http.StatusBadRequest, "invalid", "limit: 1 to "+strconv.Itoa(listMax))
			return
		}
		limit = n
	}
	out, more := []runView{}, false
	for _, run := range list {
		if (mine && run.Submitter != id.Subject) || (state != "" && string(run.State) != state) {
			continue
		}
		if len(out) == limit {
			more = true
			break
		}
		out = append(out, view(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": out, "more": more})
}

// listMax is the most runs GET /v1/applications returns, and the most it looks through.
const listMax = 200

// specView is GET /v1/applications/{id}/spec: the run's spec as POST /v1/applications takes it, to
// submit a new run with the same settings ("Run again"). Its inline code is there until
// sessions.resultRetention clears it; then CodeCleared is true and the spec has no code.
type specView struct {
	Spec        runs.Spec `json:"spec"`
	CodeCleared bool      `json:"codeCleared"`
}

// spec is for whoever may read the run's log (mayManage): the spec carries its code and arguments.
func (a Applications) spec(w http.ResponseWriter, r *http.Request) {
	run, id, ok := a.load(w, r)
	if !ok {
		return
	}
	if !mayManage(id, run) {
		apiError(w, http.StatusForbidden, "forbidden", "only the run's submitter, a workspace owner or a platform operator can read its code and settings")
		return
	}
	s := run.Spec.Spec
	m := s.Main
	cleared := m.InlinePython == "" && m.InlinePythonSHA256 != "" || m.InlineScala == "" && m.InlineScalaSHA256 != ""
	s.Main.InlinePythonBytes, s.Main.InlinePythonSHA256, s.Main.InlineScalaBytes, s.Main.InlineScalaSHA256 = 0, "", 0, ""
	writeJSON(w, http.StatusOK, specView{Spec: s, CodeCleared: cleared})
}

// load returns the run named in the path if it belongs to the caller's workspace. A run in
// another workspace is indistinguishable from an unknown one.
func (a Applications) load(w http.ResponseWriter, r *http.Request) (runs.Run, auth.Identity, bool) {
	id, _ := auth.FromContext(r.Context())
	run, err := a.Store.Get(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, runs.ErrNotFound) || (err == nil && (run.Workspace != id.Workspace || run.Kind != "application")) {
		apiError(w, http.StatusNotFound, "not_found", "no such run in this workspace")
		return run, id, false
	}
	if err != nil {
		log.Printf("api: get run: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not read the run")
		return run, id, false
	}
	return run, id, true
}

func (a Applications) get(w http.ResponseWriter, r *http.Request) {
	if run, _, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, view(run))
	}
}

// endedMeanwhile covers a run that ended while its live log was read: its pods are going, so the
// read can fail, but the tail kept when it ended is there (found by the UI's Integration check,
// which polls a run's log as it finishes). A run still live with an unreadable log stays an error.
func (a Applications) endedMeanwhile(ctx context.Context, id, text string, err error) (string, error) {
	if err == nil {
		return text, nil
	}
	if now, gerr := a.Store.Get(ctx, id); gerr == nil && now.State.Terminal() {
		return a.Store.LogTail(ctx, id)
	}
	return text, err
}

// mayManage is item 3's rule for stopping a run and reading its logs: its submitter, an owner of
// its workspace, or a platform operator. Logs can carry what the run's code printed, its token
// included (step 5), so a viewer or another editor never reads them.
func mayManage(id auth.Identity, run runs.Run) bool {
	return run.Submitter == id.Subject || id.Role == auth.RoleOwner || id.Operator
}

func (a Applications) stop(w http.ResponseWriter, r *http.Request) {
	run, id, ok := a.load(w, r)
	if !ok {
		return
	}
	if !mayManage(id, run) {
		apiError(w, http.StatusForbidden, "forbidden", "only the run's submitter, a workspace owner or a platform operator can stop it")
		return
	}
	if run.State.Terminal() {
		writeJSON(w, http.StatusOK, view(run))
		return
	}
	if err := a.Store.RequestStop(r.Context(), run.ID); err != nil {
		log.Printf("api: stop run: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not stop the run")
		return
	}
	log.Printf("api: run %s stop requested by sub=%s", run.ID, id.Subject)
	run.StopRequested = true
	writeJSON(w, http.StatusAccepted, view(run))
}

func (a Applications) logs(w http.ResponseWriter, r *http.Request) {
	run, id, ok := a.load(w, r)
	if !ok {
		return
	}
	if !mayManage(id, run) {
		apiError(w, http.StatusForbidden, "forbidden", "only the run's submitter, a workspace owner or a platform operator can read its logs")
		return
	}
	if r.URL.Query().Has("from") {
		if r.URL.Query().Has("tail") {
			apiError(w, http.StatusBadRequest, "invalid", "from and tail can't be used together")
			return
		}
		a.logsFrom(w, r, run)
		return
	}
	tail := int64(500)
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 10000 {
			apiError(w, http.StatusBadRequest, "invalid", "tail: 1 to 10000 lines")
			return
		}
		tail = n
	}
	if run.ContentClearedAt != nil {
		apiError(w, http.StatusGone, "cleared", "the run's log was cleared "+run.ContentClearedAt.UTC().Format(time.RFC3339)+" (sessions.resultRetention)")
		return
	}
	var text string
	var err error
	if run.State.Terminal() {
		text, err = a.Store.LogTail(r.Context(), run.ID)
	} else if a.LiveLogs != nil && run.Launched {
		// Not before its launch: until the run's namespace and the backend's RoleBinding in it
		// exist, the API refuses the read (403), and there is no log yet anyway.
		text, err = a.LiveLogs(r.Context(), run.Namespace, tail)
		text, err = a.endedMeanwhile(r.Context(), run.ID, text, err)
	}
	if err != nil {
		log.Printf("api: logs: %v", err)
		apiError(w, http.StatusBadGateway, "unavailable", "the run's log is not readable right now")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, text)
}

// logsFrom is the log cursor (ADR 0119 amendment 3): the driver's log from byte offset ?from=, at
// most runs.LogChunkBytes, with the offset to ask for next in X-Log-Offset. Offsets count bytes of
// the driver's whole log, so they mean the same while the run lives and after it ends, when what
// is left is the tail kept at its end (the last MiB): asking for an offset before that tail returns
// the tail, and X-Log-Skipped says how many bytes are gone. X-Log-Complete: true once the run has
// ended and the cursor has reached the end of its log. An offset beyond the log's end is 400.
func (a Applications) logsFrom(w http.ResponseWriter, r *http.Request, run runs.Run) {
	from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if err != nil || from < 0 {
		apiError(w, http.StatusBadRequest, "invalid", "from: a byte offset in the run's log, 0 or more")
		return
	}
	if run.ContentClearedAt != nil {
		apiError(w, http.StatusGone, "cleared", "the run's log was cleared "+run.ContentClearedAt.UTC().Format(time.RFC3339)+" (sessions.resultRetention)")
		return
	}
	ended := run.State.Terminal()
	var chunk []byte
	if !ended {
		reached := int64(0)
		if a.LiveLogFrom != nil && run.Launched {
			chunk, reached, err = a.LiveLogFrom(r.Context(), run.Namespace, from, runs.LogChunkBytes)
		}
		if err != nil || reached < from {
			// The driver is gone or the log is shorter: the run may have ended meanwhile, and then
			// what's left is the kept tail.
			if now, gerr := a.Store.Get(r.Context(), run.ID); gerr == nil && now.State.Terminal() {
				ended, err = true, nil
			}
		}
		switch {
		case ended:
		case err != nil:
			log.Printf("api: logs from %d: %v", from, err)
			apiError(w, http.StatusBadGateway, "unavailable", "the run's log is not readable right now")
			return
		case reached < from:
			apiError(w, http.StatusBadRequest, "invalid", "from: beyond the end of the run's log ("+strconv.FormatInt(reached, 10)+" bytes so far)")
			return
		}
	}
	next, skipped := from+int64(len(chunk)), int64(0)
	if ended {
		tail, off, err := a.Store.LogTailAt(r.Context(), run.ID)
		if err != nil {
			log.Printf("api: logs from %d: %v", from, err)
			apiError(w, http.StatusInternalServerError, "internal", "could not read the run's log")
			return
		}
		end := off + int64(len(tail))
		switch {
		case from > end:
			apiError(w, http.StatusBadRequest, "invalid", "from: beyond the end of the run's log ("+strconv.FormatInt(end, 10)+" bytes)")
			return
		case from < off:
			chunk, skipped = []byte(tail), off-from
		default:
			chunk = []byte(tail[from-off:])
		}
		next = end
		w.Header().Set("X-Log-Complete", "true")
	}
	w.Header().Set("X-Log-Offset", strconv.FormatInt(next, 10))
	if skipped > 0 {
		w.Header().Set("X-Log-Skipped", strconv.FormatInt(skipped, 10))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(chunk)
}

// StoreRuns resolves the Spark UI's runs from the run records: a run's UI exists while it runs.
type StoreRuns struct{ Store RunStore }

// Lookup implements RunLookup.
func (s StoreRuns) Lookup(ctx context.Context, id string) (uiproxy.Run, bool) {
	run, err := s.Store.Get(ctx, id)
	if err != nil || run.State != runs.Running {
		return uiproxy.Run{}, false
	}
	return uiproxy.Run{
		ID: run.ID, Workspace: run.Workspace, Submitter: run.Submitter, Workload: run.Workload,
		UIURL: "http://" + runs.DriverService + "." + run.Namespace + ".svc:" + strconv.Itoa(runs.UIPort),
	}, true
}
