package runs

import (
	"bytes"
	"strings"
	"testing"
	"testing/iotest"
)

func TestSanitize_KeepsEveryByteOffset(t *testing.T) {
	in := []byte("ok é \xff\xfe end")
	out := sanitize(in)
	if len(out) != len(in) || string(out) != "ok é ?? end" {
		t.Errorf("sanitize = %q (%d bytes), want the same length", out, len(out))
	}
	if valid := []byte("plain\n"); &sanitize(valid)[0] != &valid[0] {
		t.Error("valid text was copied")
	}
}

func TestCutChunk(t *testing.T) {
	cases := []struct {
		name, in string
		max      int
		want     string
	}{
		{"short chunk kept whole", "a\nb", 10, "a\nb"},
		{"full chunk ends at its last newline", "line1\nline2\npart", 16, "line1\nline2\n"},
		{"full chunk with no newline is kept", "abcdefgh", 8, "abcdefgh"},
		{"an incomplete character is left for the next read", "ab\xc3", 10, "ab"},
		{"an incomplete 3-byte character", "ab\xe2\x82", 10, "ab"},
		{"a complete character stays", "abé", 10, "abé"},
	}
	for _, c := range cases {
		if got := string(cutChunk([]byte(c.in), c.max)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestReadFrom(t *testing.T) {
	log := "first line\nsecond line\nthird line\n"
	got, reached, err := readFrom(strings.NewReader(log), 11, 100)
	if err != nil || reached != 11 || string(got) != "second line\nthird line\n" {
		t.Errorf("from 11: %q %d %v", got, reached, err)
	}
	// Reading on from where the last read stopped joins up exactly.
	a, _, _ := readFrom(strings.NewReader(log), 0, 15)
	b, _, _ := readFrom(strings.NewReader(log), int64(len(a)), 100)
	if string(a)+string(b) != log || string(a) != "first line\n" {
		t.Errorf("two reads: %q + %q", a, b)
	}
	// At the end: nothing new, offset unchanged.
	if got, reached, _ := readFrom(strings.NewReader(log), int64(len(log)), 100); len(got) != 0 || reached != int64(len(log)) {
		t.Errorf("at the end: %q %d", got, reached)
	}
	// Beyond the end: reports how far the log reached.
	if got, reached, _ := readFrom(strings.NewReader(log), 1000, 100); got != nil || reached != int64(len(log)) {
		t.Errorf("beyond the end: %q %d", got, reached)
	}
}

func TestKeepTail(t *testing.T) {
	var log bytes.Buffer
	for i := 0; i < 5000; i++ {
		log.WriteString("a log line that is about forty bytes...\n")
	}
	whole := log.String()
	tail, off, err := keepTail(iotest.HalfReader(strings.NewReader(whole)), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) > 1000 || off+int64(len(tail)) != int64(len(whole)) || whole[off:] != string(tail) {
		t.Fatalf("tail of %d bytes at %d doesn't match the log's end", len(tail), off)
	}
	if off == 0 || whole[off-1] != '\n' {
		t.Errorf("the kept tail doesn't start at a line (offset %d)", off)
	}
	// A short log is kept whole, at offset 0.
	if tail, off, _ := keepTail(strings.NewReader("short\n"), 1000); string(tail) != "short\n" || off != 0 {
		t.Errorf("short log: %q %d", tail, off)
	}
}
