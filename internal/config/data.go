package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/projectbooth/booth-spark/internal/runs"
)

// DataAccess is the chart's `dataAccess` values (BOOTH_DATA_ACCESS) plus what core and the chart
// deliver alongside: the minting credential (booth-workload-minting-credentials), the backend's
// own image (for the agent) and its internal URL. Nil when data access is off.
type DataAccess struct {
	Enabled bool   `json:"enabled"`
	CoreURL string `json:"coreUrl"`
	Sidecar struct {
		Image string `json:"image"`
	} `json:"sidecar"`
	Database struct {
		Enabled bool            `json:"enabled"`
		Egress  runs.EgressPeer `json:"egress"`
	} `json:"database"`
	Lakehouse struct {
		Enabled bool `json:"enabled"`
	} `json:"lakehouse"`
	Storage struct {
		Enabled bool `json:"enabled"`
	} `json:"storage"`
	ObjectStore struct {
		Egress runs.EgressPeer `json:"egress"`
	} `json:"objectStore"`
	// RefreshMax: re-mint tokens, and re-fetch them in run pods, at least this often (a test knob).
	RefreshMax string `json:"refreshMax"`

	RefreshMaxD    time.Duration `json:"-"`
	MintURL        string        `json:"-"` // BOOTH_WORKLOAD_MINT_URL
	MintCredential string        `json:"-"` // BOOTH_WORKLOAD_MINT_CREDENTIAL
	AgentImage     string        `json:"-"` // BOOTH_AGENT_IMAGE
	InternalAddr   string        `json:"-"` // BOOTH_INTERNAL_ADDR
	InternalURL    string        `json:"-"` // BOOTH_INTERNAL_URL
	InternalPort   int32         `json:"-"`
}

// Limits is which data paths runs are offered.
func (d *DataAccess) Limits() runs.DataLimits {
	if d == nil {
		return runs.DataLimits{}
	}
	return runs.DataLimits{Database: d.Database.Enabled, Lakehouse: d.Lakehouse.Enabled, Storage: d.Storage.Enabled}
}

func loadData(raw string) (*DataAccess, error) {
	var d DataAccess
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	if !d.Enabled {
		return nil, nil
	}
	if d.CoreURL == "" {
		return nil, fmt.Errorf("dataAccess.coreUrl is required")
	}
	if !strings.Contains(d.Sidecar.Image, "@sha256:") {
		return nil, fmt.Errorf("dataAccess.sidecar.image must be pinned by digest")
	}
	if d.RefreshMax != "" {
		v, err := time.ParseDuration(d.RefreshMax)
		if err != nil || v < time.Second {
			return nil, fmt.Errorf("dataAccess.refreshMax %q", d.RefreshMax)
		}
		d.RefreshMaxD = v
	}
	for _, p := range []struct {
		name string
		peer runs.EgressPeer
		on   bool
	}{{"dataAccess.database.egress", d.Database.Egress, d.Database.Enabled}, {"dataAccess.objectStore.egress", d.ObjectStore.Egress, d.Lakehouse.Enabled || d.Storage.Enabled}} {
		if p.on && len(p.peer.PodSelector) > 0 && (p.peer.Port <= 0 || p.peer.Port > 65535) {
			return nil, fmt.Errorf("%s.port", p.name)
		}
	}
	d.MintURL = os.Getenv("BOOTH_WORKLOAD_MINT_URL")
	d.MintCredential = os.Getenv("BOOTH_WORKLOAD_MINT_CREDENTIAL")
	d.AgentImage = os.Getenv("BOOTH_AGENT_IMAGE")
	d.InternalAddr = getEnv("BOOTH_INTERNAL_ADDR", ":8081")
	d.InternalURL = os.Getenv("BOOTH_INTERNAL_URL")
	for k, v := range map[string]string{"BOOTH_WORKLOAD_MINT_URL": d.MintURL, "BOOTH_WORKLOAD_MINT_CREDENTIAL": d.MintCredential,
		"BOOTH_AGENT_IMAGE": d.AgentImage, "BOOTH_INTERNAL_URL": d.InternalURL} {
		if v == "" {
			return nil, fmt.Errorf("%s is required with data access on", k)
		}
	}
	_, port, ok := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(d.InternalURL, "http://"), "https://"), ":")
	var p int
	if !ok || func() bool { _, err := fmt.Sscanf(port, "%d", &p); return err != nil }() || p <= 0 || p > 65535 {
		return nil, fmt.Errorf("BOOTH_INTERNAL_URL %q has no port", d.InternalURL)
	}
	d.InternalPort = int32(p)
	return &d, nil
}
