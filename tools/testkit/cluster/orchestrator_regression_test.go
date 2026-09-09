package cluster

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/im-pingo/liveforge/config"
)

func TestGeneratedNodeConfigPassesProductionLoader(t *testing.T) {
	topology := OriginEdge("rtmp")
	ports := map[string]AllocatedPorts{
		"origin": {Ports: map[string]int{"rtmp": 19350, "api": 18090}},
		"edge":   {Ports: map[string]int{"rtmp": 19360, "api": 18091}},
	}
	for _, node := range topology.Nodes {
		t.Run(node.Name, func(t *testing.T) {
			generated := GenerateNodeConfig(node, ports[node.Name], topology.Links, ports)
			path, err := WriteConfig(generated, t.TempDir(), node.Name+".yaml")
			if err != nil {
				t.Fatal(err)
			}
			if _, loadErr := config.Load(path); loadErr != nil {
				t.Fatalf("generated node cannot start: %v", loadErr)
			}
		})
	}
}

func TestFetchStreamsReadsManagementAPIEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		keys       []string
	}{
		{"active", `{"code":0,"message":"ok","data":{"streams":[{"key":"live/test"}]}}`, []string{"live/test"}},
		{"empty", `{"code":0,"message":"ok","data":{"streams":[]}}`, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := fmt.Fprint(w, tc.body); err != nil {
					t.Errorf("write streams fixture: %v", err)
				}
			}))
			defer server.Close()
			keys, err := fetchStreams(server.Client(), server.URL)
			if err != nil || !reflect.DeepEqual(keys, tc.keys) {
				t.Fatalf("keys=%v error=%v, want%v", keys, err, tc.keys)
			}
		})
	}
}
