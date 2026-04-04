package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/analyzer"
	"github.com/im-pingo/liveforge/tools/testkit/play"
	"github.com/im-pingo/liveforge/tools/testkit/push"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

// ClusterTestConfig holds parameters for a cluster integration test.
type ClusterTestConfig struct {
	BinaryPath   string        // path to liveforge binary
	Topology     *Topology     // cluster topology to deploy
	PushProtocol string        // protocol for pushing to origin (e.g. "rtmp")
	PlayProtocol string        // protocol for playing from edge (e.g. "rtmp")
	StreamKey    string        // stream key including app, e.g. "live/test"
	Duration     time.Duration // play duration
}

// nodeProcess tracks a running liveforge server process.
type nodeProcess struct {
	name    string
	cmd     *exec.Cmd
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	ports   AllocatedPorts
	apiPort int
}

// Orchestrator manages the lifecycle of a multi-node cluster test.
type Orchestrator struct {
	binaryPath string
}

// NewOrchestrator creates an Orchestrator that uses the given liveforge binary.
func NewOrchestrator(binaryPath string) *Orchestrator {
	return &Orchestrator{binaryPath: binaryPath}
}

// Run executes the full cluster test:
//  1. Allocate ports for all nodes
//  2. Generate per-node YAML configs
//  3. Start each node process (origin first, then others)
//  4. Wait for all nodes to become healthy
//  5. Push a stream to the origin node
//  6. Wait for stream propagation to the edge
//  7. Play from the edge and analyze
//  8. Stop push/play
//  9. Stop all node processes (SIGTERM, then SIGKILL after 5s)
//  10. Return a ClusterReport
func (o *Orchestrator) Run(ctx context.Context, cfg ClusterTestConfig) (*report.ClusterReport, error) {
	// Step 1: Allocate ports.
	allPorts, err := allocateAllPorts(cfg.Topology)
	if err != nil {
		return nil, fmt.Errorf("allocate ports: %w", err)
	}

	// Step 2: Generate configs and write to temp dir.
	tmpDir, err := os.MkdirTemp("", "lf-cluster-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	configPaths := make(map[string]string)
	for _, node := range cfg.Topology.Nodes {
		nodePorts := allPorts[node.Name]
		nodeCfg := GenerateNodeConfig(node, nodePorts, cfg.Topology.Links, allPorts)
		path, err := WriteConfig(nodeCfg, tmpDir, node.Name+".yaml")
		if err != nil {
			return nil, fmt.Errorf("write config for %s: %w", node.Name, err)
		}
		configPaths[node.Name] = path
	}

	// Step 3: Start node processes (origin first).
	processes, err := o.startNodes(ctx, cfg.Topology, allPorts, configPaths)
	if err != nil {
		stopAll(processes)
		return nil, fmt.Errorf("start nodes: %w", err)
	}
	defer stopAll(processes)

	// Step 4: Wait for all nodes to become healthy.
	nodeStatuses, err := waitAllHealthy(ctx, processes)
	if err != nil {
		return nil, fmt.Errorf("health check: %w", err)
	}

	// Find origin and edge nodes.
	originNode, edgeNode, err := findEndpoints(cfg.Topology)
	if err != nil {
		return nil, err
	}

	originPorts := allPorts[originNode.Name]
	edgePorts := allPorts[edgeNode.Name]

	// Step 5: Push a stream to the origin.
	pusher, err := push.NewPusher(cfg.PushProtocol)
	if err != nil {
		return nil, fmt.Errorf("create pusher: %w", err)
	}

	// Use a looping source so push does not run out of frames before play finishes.
	src := source.NewFLVSourceLoop(0)

	pushURL := buildStreamURL(cfg.PushProtocol, originPorts.Ports[cfg.PushProtocol], cfg.StreamKey)
	pushCfg := push.PushConfig{
		Protocol: cfg.PushProtocol,
		Target:   pushURL,
		Duration: cfg.Duration + 10*time.Second, // push slightly longer than play
	}

	pushCtx, pushCancel := context.WithCancel(ctx)
	defer pushCancel()

	type pushResult struct {
		report *report.PushReport
		err    error
	}
	pushCh := make(chan pushResult, 1)
	go func() {
		rpt, err := pusher.Push(pushCtx, src, pushCfg)
		pushCh <- pushResult{rpt, err}
	}()

	// Step 6: Wait for stream to propagate to edge.
	edgeAPIPort := allPorts[edgeNode.Name].Ports["api"]
	relayStart := time.Now()
	if err := waitStreamPropagation(ctx, edgeAPIPort, cfg.StreamKey); err != nil {
		pushCancel()
		return nil, fmt.Errorf("stream propagation: %w", err)
	}
	relayMs := time.Since(relayStart).Milliseconds()

	// Step 7: Play from edge and analyze.
	player, err := play.NewPlayer(cfg.PlayProtocol)
	if err != nil {
		pushCancel()
		return nil, fmt.Errorf("create player: %w", err)
	}

	playURL := buildStreamURL(cfg.PlayProtocol, edgePorts.Ports[cfg.PlayProtocol], cfg.StreamKey)
	playCfg := play.PlayConfig{
		Protocol: cfg.PlayProtocol,
		URL:      playURL,
		Duration: cfg.Duration,
	}

	az := analyzer.New()
	if err := player.Play(ctx, playCfg, az.Feed); err != nil {
		pushCancel()
		return nil, fmt.Errorf("play from edge: %w", err)
	}
	playReport := az.Report()

	// Step 8: Stop push.
	pushCancel()
	pushRes := <-pushCh

	// Build report.
	rpt := &report.ClusterReport{
		Topology: cfg.Topology.Name,
		Nodes:    nodeStatuses,
		Push:     pushRes.report,
		Play:     playReport,
		RelayMs:  relayMs,
	}

	return rpt, nil
}

// startNodes launches liveforge processes for all nodes in the topology.
// Origin nodes are started first to ensure they are ready for edge connections.
func (o *Orchestrator) startNodes(ctx context.Context, topo *Topology, allPorts map[string]AllocatedPorts, configPaths map[string]string) ([]*nodeProcess, error) {
	// Sort: origins first, then centers, then edges.
	ordered := orderByRole(topo.Nodes)
	var processes []*nodeProcess

	for _, node := range ordered {
		ports := allPorts[node.Name]
		configPath := configPaths[node.Name]

		proc, err := o.startNode(ctx, node.Name, configPath, ports)
		if err != nil {
			return processes, fmt.Errorf("start node %s: %w", node.Name, err)
		}
		processes = append(processes, proc)
	}

	return processes, nil
}

// startNode launches a single liveforge process.
func (o *Orchestrator) startNode(ctx context.Context, name, configPath string, ports AllocatedPorts) (*nodeProcess, error) {
	cmd := exec.CommandContext(ctx, o.binaryPath, "-c", configPath)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %s: %w", o.binaryPath, err)
	}

	apiPort := ports.Ports["api"]
	return &nodeProcess{
		name:    name,
		cmd:     cmd,
		stdout:  &stdout,
		stderr:  &stderr,
		ports:   ports,
		apiPort: apiPort,
	}, nil
}

// waitAllHealthy polls each node's health endpoint until all respond healthy.
func waitAllHealthy(ctx context.Context, processes []*nodeProcess) ([]report.NodeStatus, error) {
	timeout := 10 * time.Second
	deadline := time.Now().Add(timeout)

	statuses := make([]report.NodeStatus, len(processes))
	for i, proc := range processes {
		statuses[i] = report.NodeStatus{
			Name:  proc.name,
			Ports: proc.ports.Ports,
		}
	}

	for i, proc := range processes {
		if err := waitNodeHealthy(ctx, proc.apiPort, deadline); err != nil {
			statuses[i].Healthy = false
			// Return partial statuses with the error.
			return statuses, fmt.Errorf("node %s not healthy: %w", proc.name, err)
		}
		statuses[i].Healthy = true
	}

	return statuses, nil
}

// waitNodeHealthy polls a single node's health endpoint.
func waitNodeHealthy(ctx context.Context, apiPort int, deadline time.Time) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/server/health", apiPort)
	client := &http.Client{Timeout: 2 * time.Second}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("health check timed out after 10s on port %d", apiPort)
			}

			resp, err := client.Get(url)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

// waitStreamPropagation polls the edge node's stream list until the target
// stream key appears.
func waitStreamPropagation(ctx context.Context, edgeAPIPort int, streamKey string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/streams", edgeAPIPort)
	client := &http.Client{Timeout: 2 * time.Second}

	timeout := 15 * time.Second
	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("stream %q not found on edge after %v", streamKey, timeout)
			}

			streams, err := fetchStreams(client, url)
			if err != nil {
				continue
			}

			for _, s := range streams {
				if s == streamKey || strings.HasSuffix(s, "/"+streamKey) || strings.Contains(s, streamKey) {
					return nil
				}
			}
		}
	}
}

// fetchStreams calls the streams API and returns a list of stream keys.
func fetchStreams(client *http.Client, url string) ([]string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("streams API returned %d", resp.StatusCode)
	}

	// The API can return either an array of strings or an array of objects
	// with a "key" field. Try both.
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode streams response: %w", err)
	}

	// Try array of strings.
	var keys []string
	if err := json.Unmarshal(raw, &keys); err == nil {
		return keys, nil
	}

	// Try array of objects with "key" or "name" field.
	var objects []map[string]any
	if err := json.Unmarshal(raw, &objects); err == nil {
		var result []string
		for _, obj := range objects {
			if k, ok := obj["key"].(string); ok {
				result = append(result, k)
			} else if n, ok := obj["name"].(string); ok {
				result = append(result, n)
			}
		}
		return result, nil
	}

	return nil, fmt.Errorf("unexpected streams response format")
}

// stopAll sends SIGTERM to all processes and waits up to 5 seconds for graceful
// shutdown. If a process does not exit in time, it is killed with SIGKILL.
func stopAll(processes []*nodeProcess) {
	for _, proc := range processes {
		if proc.cmd.Process == nil {
			continue
		}
		_ = proc.cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		for _, proc := range processes {
			if proc.cmd.Process == nil {
				continue
			}
			_ = proc.cmd.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		for _, proc := range processes {
			if proc.cmd.Process == nil {
				continue
			}
			_ = proc.cmd.Process.Kill()
		}
	}
}

// allocateAllPorts allocates ports for every node in the topology.
// Each node gets ports for its declared protocols plus an API port.
func allocateAllPorts(topo *Topology) (map[string]AllocatedPorts, error) {
	result := make(map[string]AllocatedPorts, len(topo.Nodes))

	for _, node := range topo.Nodes {
		ports := AllocatedPorts{Ports: make(map[string]int)}

		// Allocate ports for each declared protocol.
		for _, proto := range node.Protocols {
			port, err := allocPort(proto)
			if err != nil {
				return nil, fmt.Errorf("alloc %s port for %s: %w", proto, node.Name, err)
			}
			ports.Ports[proto] = port
		}

		// Always allocate an API port.
		apiPort, err := allocPort("api")
		if err != nil {
			return nil, fmt.Errorf("alloc api port for %s: %w", node.Name, err)
		}
		ports.Ports["api"] = apiPort

		result[node.Name] = ports
	}

	return result, nil
}

// allocPort allocates a free port by binding to :0 and immediately releasing.
// SRT uses UDP; all other protocols use TCP.
func allocPort(proto string) (int, error) {
	if proto == "srt" {
		return allocUDPPort()
	}
	return allocTCPPort()
}

// allocTCPPort binds a TCP listener on 127.0.0.1:0 and returns the assigned port.
func allocTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen tcp: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// allocUDPPort binds a UDP socket on 127.0.0.1:0 and returns the assigned port.
func allocUDPPort() (int, error) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen udp: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port, nil
}

// orderByRole sorts nodes so origins come first, then centers, then edges.
// This ensures upstream nodes are started before downstream ones.
func orderByRole(nodes []NodeConfig) []NodeConfig {
	result := make([]NodeConfig, 0, len(nodes))
	for _, n := range nodes {
		if n.Role == RoleOrigin {
			result = append(result, n)
		}
	}
	for _, n := range nodes {
		if n.Role == RoleCenter {
			result = append(result, n)
		}
	}
	for _, n := range nodes {
		if n.Role == RoleEdge {
			result = append(result, n)
		}
	}
	return result
}

// findEndpoints returns the first origin and last edge node from the topology.
func findEndpoints(topo *Topology) (origin, edge NodeConfig, err error) {
	var foundOrigin, foundEdge bool
	for _, n := range topo.Nodes {
		if n.Role == RoleOrigin && !foundOrigin {
			origin = n
			foundOrigin = true
		}
		if n.Role == RoleEdge {
			edge = n
			foundEdge = true
		}
	}
	if !foundOrigin {
		return origin, edge, fmt.Errorf("no origin node in topology")
	}
	if !foundEdge {
		return origin, edge, fmt.Errorf("no edge node in topology")
	}
	return origin, edge, nil
}

// buildStreamURL constructs a full stream URL for push or play.
func buildStreamURL(proto string, port int, streamKey string) string {
	scheme := protocolScheme(proto)
	return fmt.Sprintf("%s://127.0.0.1:%d/%s", scheme, port, streamKey)
}

// findBinary locates the liveforge binary. It checks:
// 1. $LF_BINARY environment variable
// 2. ./bin/liveforge relative to the working directory
// 3. $(go env GOPATH)/bin/liveforge
func findBinary(t *testing.T) string {
	t.Helper()

	// Check environment variable.
	if bin := os.Getenv("LF_BINARY"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}

	// Check ./bin/liveforge.
	if bin, err := filepath.Abs("bin/liveforge"); err == nil {
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}

	// Check $GOPATH/bin/liveforge.
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	if gopath != "" {
		bin := filepath.Join(gopath, "bin", "liveforge")
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}

	t.Skip("liveforge binary not found; set LF_BINARY or build to ./bin/liveforge")
	return ""
}
