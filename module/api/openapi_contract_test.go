package api

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type openAPIContract struct {
	Servers    []openAPIServer           `yaml:"servers"`
	Paths      map[string]openAPIPath    `yaml:"paths"`
	Components openAPIContractComponents `yaml:"components"`
}

type openAPIServer struct {
	URL       string                           `yaml:"url"`
	Variables map[string]openAPIServerVariable `yaml:"variables"`
}

type openAPIServerVariable struct {
	Default string `yaml:"default"`
}

type openAPIPath struct {
	Servers    []openAPIServer    `yaml:"servers"`
	Parameters []openAPIParameter `yaml:"parameters"`
	Get        *openAPIOperation  `yaml:"get"`
	Post       *openAPIOperation  `yaml:"post"`
	Delete     *openAPIOperation  `yaml:"delete"`
	Patch      *openAPIOperation  `yaml:"patch"`
	Options    *openAPIOperation  `yaml:"options"`
}

type openAPIOperation struct {
	Servers    []openAPIServer    `yaml:"servers"`
	Parameters []openAPIParameter `yaml:"parameters"`
	Responses  map[string]any     `yaml:"responses"`
}

type openAPIParameter struct {
	Ref         string `yaml:"$ref"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type openAPIContractComponents struct {
	Parameters map[string]openAPIParameter `yaml:"parameters"`
}

func TestOpenAPIListenerOwnership(t *testing.T) {
	doc := loadOpenAPIContract(t)

	for path, item := range doc.Paths {
		for method, operation := range item.operations() {
			if operation == nil {
				continue
			}
			servers := operation.Servers
			if len(servers) == 0 {
				servers = item.Servers
			}
			if len(servers) == 0 {
				servers = doc.Servers
			}

			want := "http://127.0.0.1:8090"
			if strings.HasPrefix(path, "/webrtc/") {
				want = "http://127.0.0.1:8443"
			}
			if len(servers) != 1 || servers[0].resolvedURL() != want {
				t.Errorf("%s %s resolves to servers %v, want only %s", method, path, resolvedServerURLs(servers), want)
			}
		}
	}
}

func TestOpenAPIIncludesMiddlewareResponseStatuses(t *testing.T) {
	doc := loadOpenAPIContract(t)
	tests := []struct {
		method string
		path   string
		want   []string
	}{
		{method: "GET", path: "/api/v1/server/health", want: []string{"200", "429"}},
		{method: "POST", path: "/api/relay/push", want: []string{"200", "400", "401", "403", "404", "406", "429", "503"}},
		{method: "POST", path: "/api/relay/pull", want: []string{"200", "400", "401", "403", "404", "406", "429"}},
		{method: "POST", path: "/api/relay/gb/push", want: []string{"200", "400", "401", "403", "429", "503"}},
		{method: "POST", path: "/api/relay/gb/pull", want: []string{"200", "400", "401", "403", "404", "429"}},
		{method: "POST", path: "/webrtc/whip/{streamKey}", want: []string{"201", "400", "401", "413", "415", "429", "500", "503"}},
		{method: "POST", path: "/webrtc/whep/{streamKey}", want: []string{"201", "400", "401", "404", "413", "415", "429", "500", "503"}},
		{method: "OPTIONS", path: "/webrtc/whip/{streamKey}", want: []string{"204", "429"}},
		{method: "OPTIONS", path: "/webrtc/whep/{streamKey}", want: []string{"204", "429"}},
		{method: "OPTIONS", path: "/webrtc/session/{sessionId}", want: []string{"204", "429"}},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			operation := doc.Paths[tt.path].operation(tt.method)
			if operation == nil {
				t.Fatalf("operation is not documented")
			}
			got := make([]string, 0, len(operation.Responses))
			for status := range operation.Responses {
				got = append(got, status)
			}
			sort.Strings(got)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("response statuses = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenAPICatalogAcceptsChannelOrDeviceID(t *testing.T) {
	doc := loadOpenAPIContract(t)
	const path = "/api/v1/gb28181/channels/{channelOrDeviceId}/catalog"
	item, ok := doc.Paths[path]
	if !ok {
		t.Fatalf("catalog path %s is not documented", path)
	}
	if len(item.Parameters) != 1 {
		t.Fatalf("catalog path parameters = %d, want 1", len(item.Parameters))
	}
	parameter := item.Parameters[0]
	if parameter.Ref != "" {
		const prefix = "#/components/parameters/"
		if !strings.HasPrefix(parameter.Ref, prefix) {
			t.Fatalf("unsupported parameter reference %q", parameter.Ref)
		}
		parameter = doc.Components.Parameters[strings.TrimPrefix(parameter.Ref, prefix)]
	}
	if parameter.Name != "channelOrDeviceId" {
		t.Errorf("catalog parameter name = %q, want channelOrDeviceId", parameter.Name)
	}
	description := strings.ToLower(parameter.Description)
	if !strings.Contains(description, "channel") || !strings.Contains(description, "device") {
		t.Errorf("catalog parameter description must name both channel and device IDs: %q", parameter.Description)
	}
}

func TestOpenAPIInternalReferencesResolve(t *testing.T) {
	data, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}

	refs := 0
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "$ref" {
					ref, ok := child.(string)
					if !ok || !strings.HasPrefix(ref, "#/") {
						t.Errorf("unsupported reference %v", child)
						continue
					}
					refs++
					if _, err := resolveYAMLPointer(document, ref); err != nil {
						t.Error(err)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(document)
	if refs == 0 {
		t.Fatal("OpenAPI document contains no internal references")
	}
}

func loadOpenAPIContract(t *testing.T) openAPIContract {
	t.Helper()
	data, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc openAPIContract
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	return doc
}

func (p openAPIPath) operations() map[string]*openAPIOperation {
	return map[string]*openAPIOperation{
		"GET": p.Get, "POST": p.Post, "DELETE": p.Delete,
		"PATCH": p.Patch, "OPTIONS": p.Options,
	}
}

func (p openAPIPath) operation(method string) *openAPIOperation {
	return p.operations()[method]
}

func (s openAPIServer) resolvedURL() string {
	url := s.URL
	for name, variable := range s.Variables {
		url = strings.ReplaceAll(url, "{"+name+"}", variable.Default)
	}
	return url
}

func resolvedServerURLs(servers []openAPIServer) []string {
	urls := make([]string, 0, len(servers))
	for _, server := range servers {
		urls = append(urls, server.resolvedURL())
	}
	return urls
}

func resolveYAMLPointer(document any, ref string) (any, error) {
	current := document
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reference %q crosses a non-object", ref)
		}
		current, ok = object[part]
		if !ok {
			return nil, fmt.Errorf("reference %q does not resolve", ref)
		}
	}
	return current, nil
}
