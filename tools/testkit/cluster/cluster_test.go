package cluster

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestClusterOriginEdge runs a full origin->edge cluster test.
// It requires a compiled liveforge binary and is skipped in short mode.
func TestClusterOriginEdge(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster test requires liveforge binary; skipping in short mode")
	}

	binary := findBinary(t)

	cfg := ClusterTestConfig{
		BinaryPath:   binary,
		Topology:     OriginEdge("rtmp"),
		PushProtocol: "rtmp",
		PlayProtocol: "rtmp",
		StreamKey:    "live/test",
		Duration:     5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rpt, err := NewOrchestrator(cfg.BinaryPath).Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if rpt.Play == nil {
		t.Fatal("play report is nil")
	}
	if rpt.Play.Video.FrameCount == 0 {
		t.Error("no video frames received through cluster relay")
	}
	if rpt.Push == nil {
		t.Error("push report is nil")
	}
	if rpt.RelayMs <= 0 {
		t.Error("relay latency not measured")
	}

	// Verify all nodes reported healthy.
	for _, node := range rpt.Nodes {
		if !node.Healthy {
			t.Errorf("node %s was not healthy", node.Name)
		}
	}
}

// TestTopologyOriginEdge verifies the OriginEdge topology structure.
func TestTopologyOriginEdge(t *testing.T) {
	topo := OriginEdge("rtmp")

	if topo.Name != "origin-edge" {
		t.Errorf("name = %q, want %q", topo.Name, "origin-edge")
	}
	if len(topo.Nodes) != 2 {
		t.Fatalf("node count = %d, want 2", len(topo.Nodes))
	}
	if topo.Nodes[0].Role != RoleOrigin {
		t.Errorf("first node role = %q, want %q", topo.Nodes[0].Role, RoleOrigin)
	}
	if topo.Nodes[1].Role != RoleEdge {
		t.Errorf("second node role = %q, want %q", topo.Nodes[1].Role, RoleEdge)
	}
	if len(topo.Links) != 1 {
		t.Fatalf("link count = %d, want 1", len(topo.Links))
	}
	if topo.Links[0].From != "origin" || topo.Links[0].To != "edge" {
		t.Errorf("link = %s->%s, want origin->edge", topo.Links[0].From, topo.Links[0].To)
	}
}

// TestTopologyOriginMultiEdge verifies the OriginMultiEdge topology structure.
func TestTopologyOriginMultiEdge(t *testing.T) {
	topo := OriginMultiEdge("rtmp", 3)

	if len(topo.Nodes) != 4 {
		t.Fatalf("node count = %d, want 4 (1 origin + 3 edges)", len(topo.Nodes))
	}
	if len(topo.Links) != 3 {
		t.Fatalf("link count = %d, want 3", len(topo.Links))
	}
	for i, link := range topo.Links {
		if link.From != "origin" {
			t.Errorf("link[%d].From = %q, want %q", i, link.From, "origin")
		}
	}
}

// TestTopologyOriginCenterEdge verifies the three-tier topology.
func TestTopologyOriginCenterEdge(t *testing.T) {
	topo := OriginCenterEdge("srt")

	if len(topo.Nodes) != 3 {
		t.Fatalf("node count = %d, want 3", len(topo.Nodes))
	}
	if topo.Nodes[1].Role != RoleCenter {
		t.Errorf("middle node role = %q, want %q", topo.Nodes[1].Role, RoleCenter)
	}
	if len(topo.Links) != 2 {
		t.Fatalf("link count = %d, want 2", len(topo.Links))
	}
	if topo.Links[0].From != "origin" || topo.Links[0].To != "center" {
		t.Errorf("link[0] = %s->%s, want origin->center", topo.Links[0].From, topo.Links[0].To)
	}
	if topo.Links[1].From != "center" || topo.Links[1].To != "edge" {
		t.Errorf("link[1] = %s->%s, want center->edge", topo.Links[1].From, topo.Links[1].To)
	}
}

// TestConfigGeneration verifies that GenerateNodeConfig produces correct
// forward/origin settings.
func TestConfigGeneration(t *testing.T) {
	topo := OriginEdge("rtmp")

	// Simulate allocated ports.
	allPorts := map[string]AllocatedPorts{
		"origin": {Ports: map[string]int{"rtmp": 19350, "api": 18090}},
		"edge":   {Ports: map[string]int{"rtmp": 19360, "api": 18091}},
	}

	// Generate origin config.
	originCfg := GenerateNodeConfig(topo.Nodes[0], allPorts["origin"], topo.Links, allPorts)

	if !originCfg.Cluster.Forward.Enabled {
		t.Error("origin forward.enabled should be true")
	}
	if len(originCfg.Cluster.Forward.Targets) != 1 {
		t.Fatalf("origin forward.targets count = %d, want 1", len(originCfg.Cluster.Forward.Targets))
	}
	expectedTarget := "rtmp://127.0.0.1:19360/live"
	if originCfg.Cluster.Forward.Targets[0] != expectedTarget {
		t.Errorf("origin forward.targets[0] = %q, want %q", originCfg.Cluster.Forward.Targets[0], expectedTarget)
	}
	if originCfg.Cluster.Origin.Enabled {
		t.Error("origin should not have origin.enabled = true")
	}

	// Generate edge config.
	edgeCfg := GenerateNodeConfig(topo.Nodes[1], allPorts["edge"], topo.Links, allPorts)

	if !edgeCfg.Cluster.Origin.Enabled {
		t.Error("edge origin.enabled should be true")
	}
	if len(edgeCfg.Cluster.Origin.Servers) != 1 {
		t.Fatalf("edge origin.servers count = %d, want 1", len(edgeCfg.Cluster.Origin.Servers))
	}
	expectedServer := "rtmp://127.0.0.1:19350"
	if edgeCfg.Cluster.Origin.Servers[0] != expectedServer {
		t.Errorf("edge origin.servers[0] = %q, want %q", edgeCfg.Cluster.Origin.Servers[0], expectedServer)
	}
	if edgeCfg.Cluster.Forward.Enabled {
		t.Error("edge should not have forward.enabled = true")
	}
}

// TestConfigGenerationCenterNode verifies config generation for a three-tier
// topology where the center node both forwards and pulls.
func TestConfigGenerationCenterNode(t *testing.T) {
	topo := OriginCenterEdge("rtmp")

	allPorts := map[string]AllocatedPorts{
		"origin": {Ports: map[string]int{"rtmp": 19350, "api": 18090}},
		"center": {Ports: map[string]int{"rtmp": 19360, "api": 18091}},
		"edge":   {Ports: map[string]int{"rtmp": 19370, "api": 18092}},
	}

	centerCfg := GenerateNodeConfig(topo.Nodes[1], allPorts["center"], topo.Links, allPorts)

	// Center should have both forward and origin enabled.
	if !centerCfg.Cluster.Forward.Enabled {
		t.Error("center forward.enabled should be true")
	}
	if !centerCfg.Cluster.Origin.Enabled {
		t.Error("center origin.enabled should be true")
	}

	// Forward target points to edge.
	if len(centerCfg.Cluster.Forward.Targets) != 1 {
		t.Fatalf("center forward.targets count = %d, want 1", len(centerCfg.Cluster.Forward.Targets))
	}
	if centerCfg.Cluster.Forward.Targets[0] != "rtmp://127.0.0.1:19370/live" {
		t.Errorf("center forward target = %q", centerCfg.Cluster.Forward.Targets[0])
	}

	// Origin server points to origin node.
	if len(centerCfg.Cluster.Origin.Servers) != 1 {
		t.Fatalf("center origin.servers count = %d, want 1", len(centerCfg.Cluster.Origin.Servers))
	}
	if centerCfg.Cluster.Origin.Servers[0] != "rtmp://127.0.0.1:19350" {
		t.Errorf("center origin server = %q", centerCfg.Cluster.Origin.Servers[0])
	}
}

// TestAllocPort verifies that port allocation returns valid ports.
func TestAllocPort(t *testing.T) {
	tcpPort, err := allocTCPPort()
	if err != nil {
		t.Fatalf("allocTCPPort: %v", err)
	}
	if tcpPort <= 0 || tcpPort > 65535 {
		t.Errorf("TCP port %d out of range", tcpPort)
	}

	udpPort, err := allocUDPPort()
	if err != nil {
		t.Fatalf("allocUDPPort: %v", err)
	}
	if udpPort <= 0 || udpPort > 65535 {
		t.Errorf("UDP port %d out of range", udpPort)
	}
}

// TestWriteConfig verifies YAML config serialization.
func TestWriteConfig(t *testing.T) {
	topo := OriginEdge("rtmp")
	allPorts := map[string]AllocatedPorts{
		"origin": {Ports: map[string]int{"rtmp": 19350, "api": 18090}},
		"edge":   {Ports: map[string]int{"rtmp": 19360, "api": 18091}},
	}

	cfg := GenerateNodeConfig(topo.Nodes[0], allPorts["origin"], topo.Links, allPorts)

	path, err := WriteConfig(cfg, t.TempDir(), "test-origin.yaml")
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config file: %v", err)
	}
	if fi.Size() <= 0 {
		t.Error("config file is empty")
	}
}

// TestOrderByRole verifies that node ordering puts origins first.
func TestOrderByRole(t *testing.T) {
	nodes := []NodeConfig{
		{Name: "e1", Role: RoleEdge},
		{Name: "o1", Role: RoleOrigin},
		{Name: "c1", Role: RoleCenter},
		{Name: "e2", Role: RoleEdge},
	}

	ordered := orderByRole(nodes)
	if len(ordered) != 4 {
		t.Fatalf("ordered length = %d, want 4", len(ordered))
	}
	if ordered[0].Role != RoleOrigin {
		t.Errorf("first node role = %q, want origin", ordered[0].Role)
	}
	if ordered[1].Role != RoleCenter {
		t.Errorf("second node role = %q, want center", ordered[1].Role)
	}
	if ordered[2].Role != RoleEdge {
		t.Errorf("third node role = %q, want edge", ordered[2].Role)
	}
}

// TestFindEndpoints verifies origin/edge endpoint detection.
func TestFindEndpoints(t *testing.T) {
	topo := OriginEdge("rtmp")

	origin, edge, err := findEndpoints(topo)
	if err != nil {
		t.Fatalf("findEndpoints: %v", err)
	}
	if origin.Role != RoleOrigin {
		t.Errorf("origin role = %q, want origin", origin.Role)
	}
	if edge.Role != RoleEdge {
		t.Errorf("edge role = %q, want edge", edge.Role)
	}
}

// TestBuildStreamURL verifies stream URL construction.
func TestBuildStreamURL(t *testing.T) {
	tests := []struct {
		proto     string
		port      int
		streamKey string
		want      string
	}{
		{"rtmp", 1935, "live/test", "rtmp://127.0.0.1:1935/live/test"},
		{"srt", 6000, "live/test", "srt://127.0.0.1:6000/live/test"},
		{"rtsp", 554, "live/test", "rtsp://127.0.0.1:554/live/test"},
	}

	for _, tt := range tests {
		got := buildStreamURL(tt.proto, tt.port, tt.streamKey)
		if got != tt.want {
			t.Errorf("buildStreamURL(%q, %d, %q) = %q, want %q",
				tt.proto, tt.port, tt.streamKey, got, tt.want)
		}
	}
}
