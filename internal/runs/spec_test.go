package runs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func intp(i int) *int { return &i }

func TestValidate_Defaults(t *testing.T) {
	v, err := Validate(Spec{Name: "pi", Main: Main{InlinePython: "print(1)"}}, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if v.DriverHeapMi != 512 || v.ExecutorHeapMi != 512 || v.MinExecutors != 0 || v.MaxExecutors != 2 || v.Duration != 6*time.Hour {
		t.Errorf("defaults = %+v", v)
	}
	// docs/design-v0.md item 7: a default run's largest footprint.
	if v.FootprintMi() != 3*896 {
		t.Errorf("footprint = %d", v.FootprintMi())
	}
}

func TestValidate_Refusals(t *testing.T) {
	ok := func() Spec { return Spec{Name: "pi", Main: Main{InlinePython: "print(1)"}} }
	type c struct {
		name        string
		mut         func(*Spec)
		unavailable bool
	}
	for _, tc := range []c{
		{"no name", func(s *Spec) { s.Name = "" }, false},
		{"bad name", func(s *Spec) { s.Name = "../x" }, false},
		{"no code", func(s *Spec) { s.Main.InlinePython = " " }, false},
		{"huge code", func(s *Spec) { s.Main.InlinePython = strings.Repeat("x", 300<<10) }, false},
		{"a file from storage", func(s *Spec) { s.Main.InlinePython, s.Main.Python = "", &FileRef{BackendID: "b", Path: "p.py"} }, true},
		{"a jar", func(s *Spec) {
			s.Main.InlinePython, s.Main.Jar, s.Main.MainClass = "", &FileRef{BackendID: "b", Path: "a.jar"}, "a.Main"
		}, true},
		{"data access", func(s *Spec) { s.DataAccess = &DataAccess{Database: true} }, true},
		{"too many executors", func(s *Spec) { s.Resources.Executors.Max = intp(3) }, false},
		{"min above max", func(s *Spec) { s.Resources.Executors.Min = intp(2); s.Resources.Executors.Max = intp(1) }, false},
		{"negative", func(s *Spec) { s.Resources.Executors.Min = intp(-1) }, false},
		{"too much memory", func(s *Spec) { s.Resources.Driver.Memory = "3g" }, false},
		{"too little memory", func(s *Spec) { s.Resources.Executors.Memory = "256m" }, false},
		{"not a memory size", func(s *Spec) { s.Resources.Driver.Memory = "1Gi" }, false},
		{"a module setting", func(s *Spec) { s.Conf = map[string]string{"spark.kubernetes.namespace": "kube-system"} }, false},
		{"a class name", func(s *Spec) { s.Conf = map[string]string{"spark.serializer": "x"} }, false},
		{"a multi-line value", func(s *Spec) { s.Conf = map[string]string{"spark.sql.shuffle.partitions": "1\n2"} }, false},
		{"too long", func(s *Spec) { s.MaxDuration = "7h" }, false},
		{"bad duration", func(s *Spec) { s.MaxDuration = "soon" }, false},
		{"NUL in an arg", func(s *Spec) { s.Args = []string{"a\x00"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ok()
			tc.mut(&s)
			_, err := Validate(s, testLimits())
			var u Unavailable
			var v ValidationError
			if tc.unavailable && !errors.As(err, &u) {
				t.Errorf("err = %v, want Unavailable", err)
			}
			if !tc.unavailable && !errors.As(err, &v) {
				t.Errorf("err = %v, want ValidationError", err)
			}
		})
	}
	for _, empty := range []*DataAccess{nil, {}} {
		s := ok()
		s.DataAccess = empty
		if _, err := Validate(s, testLimits()); err != nil {
			t.Errorf("dataAccess %+v refused: %v", empty, err)
		}
	}
}

func TestHeapAndPodMemory(t *testing.T) {
	for in, want := range map[string]int{"512m": 512, "1g": 1024, "2g": 2048} {
		if got, err := HeapMi(in); err != nil || got != want {
			t.Errorf("HeapMi(%q) = %d, %v", in, got, err)
		}
	}
	for heap, want := range map[int]int{512: 896, 1024: 1433, 2048: 2867} {
		if got := PodMemoryMi(heap); got != want {
			t.Errorf("PodMemoryMi(%d) = %d, want %d", heap, got, want)
		}
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != 12 || id[0] != 'r' || seen[id] || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
			t.Fatalf("bad id %q", id)
		}
		seen[id] = true
	}
}
