package runs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DataAccess is what a run asks for (docs/design-v0.md items 4 and 6): the workspace's booth-database
// schema, its booth-lakehouse warehouse, and up to MaxStorageLocations booth-storage locations.
// Everything is as the submitter, capped at editor: an editor's run reads and writes, a viewer's
// (a submitter demoted since) only reads.
type DataAccess struct {
	Database  bool              `json:"database,omitempty"`
	Lakehouse bool              `json:"lakehouse,omitempty"`
	Storage   []StorageLocation `json:"storage,omitempty"`
}

// StorageLocation is a booth-storage {backendId, path} prefix (ADR 0045) and the access asked for.
type StorageLocation struct {
	BackendID string `json:"backendId"`
	Path      string `json:"path"`
	// Access is "read" (default) or "readwrite".
	Access string `json:"access,omitempty"`
}

// FileRef is one file in booth-storage: an application's entry point.
type FileRef struct {
	BackendID string `json:"backendId"`
	Path      string `json:"path"`
}

// Any reports whether d asks for anything.
func (d *DataAccess) Any() bool {
	return d != nil && (d.Database || d.Lakehouse || len(d.Storage) > 0)
}

// DataLimits are which data paths this install has (the chart's dataAccess.*).
type DataLimits struct {
	Database, Lakehouse, Storage bool
}

// MaxStorageLocations is ADR 0045's limit on the locations one run may name.
const MaxStorageLocations = 4

const (
	maxPathLen = 1024
	// MaxMainFileBytes bounds an entry point read from booth-storage.
	MaxMainFileBytes = 256 << 20
)

var (
	backendRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	classRE   = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(\.[A-Za-z_$][A-Za-z0-9_$]*)*$`)
)

// NeedsData reports whether a run needs data access to start: it asked for some, or its entry point
// is a file in booth-storage.
func (v Validated) NeedsData() bool {
	return v.DataAccess.Any() || v.Main.Python != nil || v.Main.Jar != nil
}

// MainFile is the run's entry point in booth-storage, if it has one.
func (v Validated) MainFile() *FileRef {
	if v.Main.Python != nil {
		return v.Main.Python
	}
	return v.Main.Jar
}

// cleanStoragePath refuses anything but a plain relative path of named segments (booth-streamlit's
// rule): a location can't climb out of the prefix it names.
func cleanStoragePath(p string) error {
	switch {
	case p == "":
		return errors.New("is empty")
	case len(p) > maxPathLen:
		return fmt.Errorf("is longer than %d bytes", maxPathLen)
	case strings.HasPrefix(p, "/"):
		return errors.New("must be relative (no leading '/')")
	case strings.ContainsAny(p, "\\\x00") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return errors.New("contains a backslash or a control character")
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New(`has a "..", "." or empty segment`)
		}
	}
	return nil
}

func validateFileRef(field string, f *FileRef) error {
	if !backendRE.MatchString(f.BackendID) {
		return invalid("%s.backendId: a booth-storage backend id", field)
	}
	if err := cleanStoragePath(f.Path); err != nil {
		return invalid("%s.path %v", field, err)
	}
	if strings.HasSuffix(f.Path, "/") {
		return invalid("%s.path names a directory; it must name a file", field)
	}
	return nil
}

// validateMain checks the entry point: exactly one of inline Python, a Python file or a JAR (with
// its main class) from booth-storage.
func validateMain(m Main, l Limits) error {
	n := 0
	for _, set := range []bool{strings.TrimSpace(m.InlinePython) != "", m.Python != nil, m.Jar != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return invalid("main: exactly one of inlinePython, python or jar")
	}
	if m.InlinePython != "" {
		if len(m.InlinePython) > maxInlinePython {
			return invalid("main.inlinePython is larger than %d KiB", maxInlinePython>>10)
		}
		if m.MainClass != "" {
			return invalid("main.mainClass goes with main.jar only")
		}
		return nil
	}
	if !l.Data.Storage {
		return Unavailable{"main.python and main.jar read files from booth-storage, which this install doesn't offer (dataAccess.storage); use main.inlinePython"}
	}
	if m.Python != nil {
		if m.MainClass != "" {
			return invalid("main.mainClass goes with main.jar only")
		}
		if !strings.HasSuffix(m.Python.Path, ".py") {
			return invalid("main.python.path must name a .py file")
		}
		return validateFileRef("main.python", m.Python)
	}
	if !strings.HasSuffix(m.Jar.Path, ".jar") {
		return invalid("main.jar.path must name a .jar file")
	}
	if len(m.MainClass) > 256 || !classRE.MatchString(m.MainClass) {
		return invalid("main.mainClass: the JAR's fully qualified main class, e.g. com.example.Main")
	}
	return validateFileRef("main.jar", m.Jar)
}

// validateData checks a DataAccess against what this install offers.
func validateData(d *DataAccess, l Limits) error {
	if d == nil {
		return nil
	}
	if d.Database && !l.Data.Database {
		return Unavailable{"dataAccess.database: this install doesn't offer booth-database to runs (dataAccess.database.enabled)"}
	}
	if d.Lakehouse && !l.Data.Lakehouse {
		return Unavailable{"dataAccess.lakehouse: this install doesn't offer booth-lakehouse to runs (dataAccess.lakehouse.enabled)"}
	}
	if len(d.Storage) > 0 && !l.Data.Storage {
		return Unavailable{"dataAccess.storage: this install doesn't offer booth-storage to runs (dataAccess.storage.enabled)"}
	}
	if len(d.Storage) > MaxStorageLocations {
		return invalid("dataAccess.storage: at most %d locations", MaxStorageLocations)
	}
	seen := map[string]bool{}
	for i := range d.Storage {
		s := &d.Storage[i]
		field := fmt.Sprintf("dataAccess.storage[%d]", i)
		if err := validateFileRef(field, &FileRef{BackendID: s.BackendID, Path: strings.TrimSuffix(s.Path, "/") + "/x"}); err != nil {
			return err
		}
		if s.Access == "" {
			s.Access = "read"
		}
		if s.Access != "read" && s.Access != "readwrite" {
			return invalid("%s.access: read or readwrite", field)
		}
		key := s.BackendID + "\x00" + strings.TrimSuffix(s.Path, "/")
		if seen[key] {
			return invalid("%s names the same location twice", field)
		}
		seen[key] = true
	}
	return nil
}

// DataPlan is a run's data access as resolved by the backend before its launch (docs/design-v0.md
// item 4): the role core granted the run's token, and each location's real place in its object
// store. It holds no credential: every key and connection is the credential sidecars' own, in the
// run's pods.
type DataPlan struct {
	// Role is the run's token's role when it launched: "editor" (read and write) or "viewer"
	// (read only).
	Role string `json:"role"`
	// Database is the workspace's booth-database database name, when the run asked for it.
	Database string `json:"database,omitempty"`
	// Warehouse is the workspace's lakehouse warehouse, when the run asked for it.
	Warehouse *Location `json:"warehouse,omitempty"`
	// Storage is each declared booth-storage location, in the order asked.
	Storage []Location `json:"storage,omitempty"`
}

// Access is the credential access the plan's role allows.
func (p DataPlan) Access() string {
	if p.Role == "editor" {
		return "readwrite"
	}
	return "read"
}

// Location is a {backendId, path} prefix and where its object store keeps it, from the broker's
// answer for the run's own token.
type Location struct {
	BackendID string `json:"backendId"`
	Path      string `json:"path"`
	Access    string `json:"access"`
	Bucket    string `json:"bucket"`
	KeyPrefix string `json:"keyPrefix,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	PathStyle bool   `json:"pathStyle"`
	// Root is where the run's code finds it: s3a://<bucket>/<keyPrefix> for a storage location,
	// the warehouse's own s3://<bucket>/<prefix> for the lakehouse.
	Root string `json:"root"`
}

// ErrDataRefused wraps every reason a run can't have (or keep) its data access: the submitter lost
// access, a location isn't theirs, the workspace has no warehouse. The run fails with the reason.
var ErrDataRefused = errors.New("no data access")

// DataRefusal makes an ErrDataRefused with a reason.
func DataRefusal(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrDataRefused, fmt.Sprintf(format, a...))
}
