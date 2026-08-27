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
	Schemas    map[string]map[string]any   `yaml:"schemas"`
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
			if strings.HasPrefix(path, "/dvr/") {
				want = "http://127.0.0.1:8070"
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

func TestOpenAPIGBLabErrorsUseManagementEnvelopes(t *testing.T) {
	doc := loadOpenAPIContract(t)
	tests := []struct {
		method string
		path   string
		status string
		want   string
	}{
		{method: "POST", path: "/api/v1/gb28181/lab/sessions", status: "400", want: "#/components/responses/BadRequest"},
		{method: "DELETE", path: "/api/v1/gb28181/lab/sessions/{labSessionId}", status: "404", want: "#/components/responses/NotFound"},
	}
	for _, test := range tests {
		operation := doc.Paths[test.path].operation(test.method)
		if operation == nil {
			t.Fatalf("%s %s is not documented", test.method, test.path)
		}
		response, ok := operation.Responses[test.status].(map[string]any)
		if !ok {
			t.Fatalf("%s %s response %s = %#v", test.method, test.path, test.status, operation.Responses[test.status])
		}
		if got := response["$ref"]; got != test.want {
			t.Errorf("%s %s response %s ref = %v, want %s", test.method, test.path, test.status, got, test.want)
		}
	}
}

func TestOpenAPISIPLabRequestMatchesRuntimeValidation(t *testing.T) {
	doc := loadOpenAPIContract(t)
	const streamKeyPattern = `^(?!/)(?!.*\/$)(?!.*//)(?!\.{1,2}(?:/|$))(?!.*\/\.{1,2}(?:/|$))[\x21-\x7E]+$`
	schema := doc.Components.Schemas["SIPLabRequest"]
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("SIPLabRequest properties = %#v", schema["properties"])
	}
	tests := []struct {
		field       string
		maxLength   int
		wantPattern string
		wantWords   []string
	}{
		{field: "device_id", maxLength: 128, wantPattern: `^[A-Za-z0-9._-]+$`, wantWords: []string{"letters", "digits"}},
		{field: "stream_key", maxLength: 256, wantPattern: streamKeyPattern, wantWords: []string{"printable", "empty", "dot"}},
	}
	for _, test := range tests {
		property, ok := properties[test.field].(map[string]any)
		if !ok {
			t.Fatalf("SIPLabRequest.%s = %#v", test.field, properties[test.field])
		}
		if got := property["maxLength"]; got != test.maxLength {
			t.Errorf("SIPLabRequest.%s maxLength = %v, want %d", test.field, got, test.maxLength)
		}
		if got := property["pattern"]; got != test.wantPattern {
			t.Errorf("SIPLabRequest.%s pattern = %v, want %q", test.field, got, test.wantPattern)
		}
		description := strings.ToLower(fmt.Sprint(property["description"]))
		for _, word := range test.wantWords {
			if !strings.Contains(description, word) {
				t.Errorf("SIPLabRequest.%s description %q does not contain %q", test.field, description, word)
			}
		}
	}

	gbSchema := doc.Components.Schemas["GBLabRequest"]
	gbProperties, ok := gbSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("GBLabRequest properties = %#v", gbSchema["properties"])
	}
	streamKey, ok := gbProperties["stream_key"].(map[string]any)
	if !ok {
		t.Fatalf("GBLabRequest.stream_key = %#v", gbProperties["stream_key"])
	}
	if got := streamKey["maxLength"]; got != 256 {
		t.Errorf("GBLabRequest.stream_key maxLength = %v, want 256", got)
	}
	if got := streamKey["pattern"]; got != streamKeyPattern {
		t.Errorf("GBLabRequest.stream_key pattern = %v, want %q", got, streamKeyPattern)
	}
	description := strings.ToLower(fmt.Sprint(streamKey["description"]))
	for _, word := range []string{"printable", "empty", "dot"} {
		if !strings.Contains(description, word) {
			t.Errorf("GBLabRequest.stream_key description %q does not contain %q", description, word)
		}
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
