package api

import (
	"log"
	"net/http"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

// clusterView is GET /v1/cluster: what Spark runs on in this install, read-only. There is no
// standing Spark cluster (docs/design-v0.md items 1 and 3): each run gets its own driver pod, which
// asks Kubernetes for its executors, in its own namespace, and both end with the run. What runs
// share is the install's admission bounds (runs.maxRunning, runs.maxRunningPerWorkspace,
// runs.memoryBudget), so this shows those, how much of them the live runs hold, and the shape of a
// run's pods. Which runs are live is the applications and sessions lists, which keep their own
// visibility rules; other workspaces' runs are only counted, as a 429 at_capacity already says.
type clusterView struct {
	// Master is Spark's cluster manager: Kubernetes, through each driver's own service account.
	Master   string       `json:"master"`
	Driver   podShape     `json:"driver"`
	Executor podShape     `json:"executor"`
	Capacity capacityView `json:"capacity"`
}

// podShape is one kind of a run's pods: its default heap, its CPU request and limit, and, for
// executors, Spark's task slots per executor and the most one run may ask for.
type podShape struct {
	DefaultMemory string `json:"defaultMemory"`
	CPURequest    string `json:"cpuRequest"`
	CPULimit      string `json:"cpuLimit"`
	Cores         int    `json:"cores,omitempty"`
	MaxPerRun     int    `json:"maxPerRun,omitempty"`
}

type capacityView struct {
	MaxRunning               int `json:"maxRunning"`
	LiveRuns                 int `json:"liveRuns"`
	MaxRunningPerWorkspace   int `json:"maxRunningPerWorkspace"` // 0: no per-workspace cap
	LiveRunsInWorkspace      int `json:"liveRunsInWorkspace"`
	MemoryBudgetMi           int `json:"memoryBudgetMi"`
	MemoryInUseMi            int `json:"memoryInUseMi"`
	MemoryInUseInWorkspaceMi int `json:"memoryInUseInWorkspaceMi"`
}

// cluster serves GET /v1/cluster, for any caller with a role in the workspace (as /v1/limits).
func (a Applications) cluster(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	u, err := a.Store.Usage(r.Context(), id.Workspace)
	if err != nil {
		log.Printf("api: cluster usage: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not read the runs")
		return
	}
	writeJSON(w, http.StatusOK, clusterView{
		Master: "kubernetes",
		Driver: podShape{DefaultMemory: a.Limits.DriverMemory, CPURequest: a.DriverCPU.Request, CPULimit: a.DriverCPU.Limit},
		Executor: podShape{DefaultMemory: a.Limits.ExecutorMemory, CPURequest: a.ExecutorCPU.Request, CPULimit: a.ExecutorCPU.Limit,
			Cores: runs.ExecutorCores, MaxPerRun: a.Limits.MaxExecutors},
		Capacity: capacityView{
			MaxRunning: a.Admission.MaxRunning, LiveRuns: u.Live,
			MaxRunningPerWorkspace: a.Admission.MaxRunningPerWorkspace, LiveRunsInWorkspace: u.LiveHere,
			MemoryBudgetMi: a.Admission.MemoryBudgetMi, MemoryInUseMi: u.UsedMi, MemoryInUseInWorkspaceMi: u.UsedHereMi,
		},
	})
}
