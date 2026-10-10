package api

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

// The OpenAPI document lists exactly the /v1 operations the router serves (docs/design-v0.md
// item 6): a route added without its documentation, or documented but never served, fails here.
// CI also lints the document with @redocly/cli (ci.yml).
func TestOpenAPI_MatchesTheRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                            `yaml:"openapi"`
		Paths   map[string]map[string]interface{} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q", doc.OpenAPI)
	}
	var documented []string
	for p, ops := range doc.Paths {
		for m := range ops {
			documented = append(documented, strings.ToUpper(m)+" /v1"+p)
		}
	}
	h := NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: fakeIframe{}, SubmitMinRole: auth.RoleEditor,
		Applications: &Applications{Store: (*runs.Store)(nil)}})
	var served []string
	_ = chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(strings.ReplaceAll(route, "/*/", "/"), "/*")
		if strings.HasPrefix(route, "/v1/") && route != "/v1/openapi.yaml" {
			served = append(served, method+" "+route)
		}
		return nil
	})
	sort.Strings(documented)
	sort.Strings(served)
	if strings.Join(documented, "\n") != strings.Join(served, "\n") {
		t.Errorf("documented:\n  %s\nserved:\n  %s", strings.Join(documented, "\n  "), strings.Join(served, "\n  "))
	}
	if os.Getenv("BOOTH_TEST_OPENAPI_OUT") != "" {
		if err := os.WriteFile(os.Getenv("BOOTH_TEST_OPENAPI_OUT"), OpenAPI, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
