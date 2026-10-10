package sparkconf

import (
	"reflect"
	"regexp"
	"testing"
)

func TestUIConf(t *testing.T) {
	c := UIConf()
	for _, k := range []string{"spark.ui.killEnabled", "spark.ui.threadDumpsEnabled", "spark.ui.heapHistogramEnabled"} {
		if c[k] != "false" {
			t.Errorf("%s = %q, want false", k, c[k])
		}
	}
	// The redaction regex keeps Spark's default words and adds booth-spark's.
	re := regexp.MustCompile(c["spark.redaction.regex"])
	for _, s := range []string{"spark.hadoop.fs.s3a.secret.key", "PASSWORD", "booth.token.file", "fs.s3a.access.key", "x.bearer", "credential.path"} {
		if !re.MatchString(s) {
			t.Errorf("%q is not redacted", s)
		}
	}
}

func TestUIEnv(t *testing.T) {
	if got := UIEnv("r-1"); !reflect.DeepEqual(got, map[string]string{"APPLICATION_WEB_PROXY_BASE": "/iframe/spark/runs/r-1/ui"}) {
		t.Errorf("UIEnv = %v", got)
	}
}

func TestArgs(t *testing.T) {
	got := Args(map[string]string{"b": "2", "a": "1"})
	if want := []string{"--conf", "a=1", "--conf", "b=2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %v", got)
	}
}
