// Package cluster provides a multi-node cluster test orchestrator.
// It defines cluster topologies, generates per-node YAML configs,
// manages liveforge server processes, and verifies stream relay
// through the cluster.
package cluster

import "fmt"

// NodeRole identifies the role of a node in a cluster topology.
type NodeRole string

const (
	// RoleOrigin is the ingest node that receives published streams and
	// forwards them downstream.
	RoleOrigin NodeRole = "origin"

	// RoleCenter is a relay node that sits between origin and edge.
	RoleCenter NodeRole = "center"

	// RoleEdge is a consumer-facing node that pulls streams from upstream.
	RoleEdge NodeRole = "edge"
)

// NodeConfig describes a single node in a cluster topology.
type NodeConfig struct {
	Name      string   // unique node identifier, e.g. "origin-1"
	Role      NodeRole // origin, center, or edge
	Protocols []string // enabled protocols, e.g. ["rtmp", "http_stream"]
}

// Link describes a directed relay connection between two nodes.
type Link struct {
	From     string // source node name (the forwarder)
	To       string // target node name (the puller)
	Protocol string // relay protocol: "rtmp", "srt", etc.
}

// Topology describes a complete cluster layout with nodes and relay links.
type Topology struct {
	Name  string       // human-readable name, e.g. "origin-edge"
	Nodes []NodeConfig // all nodes in the cluster
	Links []Link       // directed relay connections
}

// OriginEdge creates a two-node topology: origin -> edge.
// The relayProto parameter specifies the protocol for the relay link
// (e.g. "rtmp", "srt").
func OriginEdge(relayProto string) *Topology {
	return &Topology{
		Name: "origin-edge",
		Nodes: []NodeConfig{
			{Name: "origin", Role: RoleOrigin, Protocols: []string{relayProto}},
			{Name: "edge", Role: RoleEdge, Protocols: []string{relayProto}},
		},
		Links: []Link{
			{From: "origin", To: "edge", Protocol: relayProto},
		},
	}
}

// OriginMultiEdge creates an origin with N edge nodes.
// Each edge connects to the origin via the specified relay protocol.
func OriginMultiEdge(relayProto string, edges int) *Topology {
	if edges < 1 {
		edges = 1
	}

	nodes := []NodeConfig{
		{Name: "origin", Role: RoleOrigin, Protocols: []string{relayProto}},
	}
	links := make([]Link, 0, edges)

	for i := 0; i < edges; i++ {
		name := fmt.Sprintf("edge-%d", i+1)
		nodes = append(nodes, NodeConfig{
			Name:      name,
			Role:      RoleEdge,
			Protocols: []string{relayProto},
		})
		links = append(links, Link{
			From:     "origin",
			To:       name,
			Protocol: relayProto,
		})
	}

	return &Topology{
		Name:  fmt.Sprintf("origin-%d-edge", edges),
		Nodes: nodes,
		Links: links,
	}
}

// OriginCenterEdge creates a three-tier topology: origin -> center -> edge.
// Both relay hops use the specified protocol.
func OriginCenterEdge(relayProto string) *Topology {
	return &Topology{
		Name: "origin-center-edge",
		Nodes: []NodeConfig{
			{Name: "origin", Role: RoleOrigin, Protocols: []string{relayProto}},
			{Name: "center", Role: RoleCenter, Protocols: []string{relayProto}},
			{Name: "edge", Role: RoleEdge, Protocols: []string{relayProto}},
		},
		Links: []Link{
			{From: "origin", To: "center", Protocol: relayProto},
			{From: "center", To: "edge", Protocol: relayProto},
		},
	}
}
