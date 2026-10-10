package runs

import (
	"errors"
	"strings"
	"testing"
)

func dataLimits() Limits {
	l := testLimits()
	l.Data = DataLimits{Database: true, Lakehouse: true, Storage: true}
	return l
}

func TestValidate_DataAccess(t *testing.T) {
	inline := Main{InlinePython: "print(1)"}
	ok := []Spec{
		{Name: "db", Main: inline, DataAccess: &DataAccess{Database: true}},
		{Name: "lake", Main: inline, DataAccess: &DataAccess{Lakehouse: true}},
		{Name: "files", Main: inline, DataAccess: &DataAccess{Storage: []StorageLocation{
			{BackendID: "lake", Path: "raw/2026"}, {BackendID: "lake", Path: "out/", Access: "readwrite"}}}},
		{Name: "py", Main: Main{Python: &FileRef{BackendID: "lake", Path: "jobs/etl.py"}}},
		{Name: "jar", Main: Main{Jar: &FileRef{BackendID: "lake", Path: "jobs/etl.jar"}, MainClass: "com.example.Etl"}},
	}
	for _, s := range ok {
		v, err := Validate(s, dataLimits())
		if err != nil {
			t.Errorf("%s: %v", s.Name, err)
			continue
		}
		if !v.NeedsData() {
			t.Errorf("%s: NeedsData is false", s.Name)
		}
	}
	if v, _ := Validate(Spec{Name: "files", Main: inline, DataAccess: &DataAccess{Storage: []StorageLocation{{BackendID: "b", Path: "p"}}}}, dataLimits()); v.DataAccess.Storage[0].Access != "read" {
		t.Errorf("default access = %q, want read", v.DataAccess.Storage[0].Access)
	}
	if v, _ := Validate(Spec{Name: "plain", Main: inline}, dataLimits()); v.NeedsData() {
		t.Error("inline Python with no data access needs data")
	}

	loc := func(path string) *DataAccess {
		return &DataAccess{Storage: []StorageLocation{{BackendID: "lake", Path: path}}}
	}
	bad := map[string]Spec{
		"two entry points":        {Name: "x", Main: Main{InlinePython: "1", Python: &FileRef{BackendID: "b", Path: "a.py"}}},
		"no entry point":          {Name: "x"},
		"a python file not .py":   {Name: "x", Main: Main{Python: &FileRef{BackendID: "b", Path: "a.sh"}}},
		"a jar without a class":   {Name: "x", Main: Main{Jar: &FileRef{BackendID: "b", Path: "a.jar"}}},
		"a class that's code":     {Name: "x", Main: Main{Jar: &FileRef{BackendID: "b", Path: "a.jar"}, MainClass: "a; rm -rf"}},
		"mainClass with python":   {Name: "x", Main: Main{InlinePython: "1", MainClass: "a.B"}},
		"a directory as a file":   {Name: "x", Main: Main{Python: &FileRef{BackendID: "b", Path: "jobs/"}}},
		"an absolute path":        {Name: "x", Main: inline, DataAccess: loc("/etc")},
		"a climbing path":         {Name: "x", Main: inline, DataAccess: loc("raw/../../other")},
		"an empty segment":        {Name: "x", Main: inline, DataAccess: loc("raw//x")},
		"a control character":     {Name: "x", Main: inline, DataAccess: loc("raw\nx")},
		"a bad backend":           {Name: "x", Main: inline, DataAccess: &DataAccess{Storage: []StorageLocation{{BackendID: "../b", Path: "p"}}}},
		"a bad access":            {Name: "x", Main: inline, DataAccess: &DataAccess{Storage: []StorageLocation{{BackendID: "b", Path: "p", Access: "admin"}}}},
		"the same location twice": {Name: "x", Main: inline, DataAccess: &DataAccess{Storage: []StorageLocation{{BackendID: "b", Path: "p"}, {BackendID: "b", Path: "p/"}}}},
		"too many locations": {Name: "x", Main: inline, DataAccess: &DataAccess{Storage: []StorageLocation{
			{BackendID: "b", Path: "1"}, {BackendID: "b", Path: "2"}, {BackendID: "b", Path: "3"}, {BackendID: "b", Path: "4"}, {BackendID: "b", Path: "5"}}}},
	}
	for name, s := range bad {
		if _, err := Validate(s, dataLimits()); !errors.As(err, new(ValidationError)) {
			t.Errorf("%s: %v, want a ValidationError", name, err)
		}
	}

	// A path this install doesn't offer is 422, not 400.
	for name, l := range map[string]DataLimits{"database": {Lakehouse: true, Storage: true}, "lakehouse": {Database: true, Storage: true}, "storage": {Database: true, Lakehouse: true}} {
		lim := testLimits()
		lim.Data = l
		s := Spec{Name: "x", Main: inline, DataAccess: &DataAccess{Database: name == "database", Lakehouse: name == "lakehouse"}}
		if name == "storage" {
			s.DataAccess.Storage = []StorageLocation{{BackendID: "b", Path: "p"}}
		}
		_, err := Validate(s, lim)
		if !errors.As(err, new(Unavailable)) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s off: %v", name, err)
		}
	}
}
