package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/beads"
)

// Draft graph construction is a pure, non-executing function over trusted
// stored read receipt observations. It performs no source reads, writes,
// queueing, or task starts: it only derives a bounded closure from the root's
// outgoing predecessor dependencies and hashes it.

const (
	// maxDraftGraphNodes bounds the closure the builder will return.
	maxDraftGraphNodes = 128
	// maxDraftGraphEdges bounds the closure's edge count.
	maxDraftGraphEdges = 512
	// maxDraftGraphObservations bounds the raw input observation count.
	maxDraftGraphObservations = 128
	// maxDraftGraphSerializedBytes bounds the canonical closure encoding.
	maxDraftGraphSerializedBytes = 2 << 20 // 2 MiB
	// draftGraphDependencyTypeBlocks is the only edge kind a draft graph
	// may carry; anything else is rejected, not downgraded.
	draftGraphDependencyTypeBlocks = "blocks"
)

// GraphObservation is one trusted stored read receipt: the item as observed,
// when it was observed, and which receipt proved it. Items are opaque native
// source data; the builder never interprets or normalizes IDs or revisions.
type GraphObservation struct {
	// ReceiptID identifies the stored read receipt proving this observation.
	ReceiptID string
	// ObservedAt is the receipt's observation time.
	ObservedAt time.Time
	// Item is the observed source item. Its DependenciesComplete flag
	// carries the receipt's completeness contract: only complete
	// observations (explicit [] for zero edges) are eligible for closure.
	Item beads.Issue
}

// DraftGraphNode is one item in the derived closure. NativeID and Revision
// are opaque source values retained verbatim.
type DraftGraphNode struct {
	NativeID   string    `json:"native_id"`
	Revision   string    `json:"revision"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	ReceiptID  string    `json:"receipt_id"`
	ObservedAt time.Time `json:"observed_at"`
}

// DraftGraphEdge is a predecessor -> consumer dependency inside the closure.
type DraftGraphEdge struct {
	PredecessorNativeID string `json:"predecessor_native_id"`
	ConsumerNativeID    string `json:"consumer_native_id"`
	DependencyType      string `json:"dependency_type"`
}

// DraftGraph is the derived non-executing draft. Digest is a SHA256 over the
// canonical scope/config/root/revisions/topology/provenance encoding; it is
// not a source CAS or an atomic snapshot of the source.
type DraftGraph struct {
	WorkspaceID    string           `json:"workspace_id"`
	SourceID       string           `json:"source_id"`
	RootNativeID   string           `json:"root_native_id"`
	ConfigRevision int32            `json:"config_revision"`
	Nodes          []DraftGraphNode `json:"nodes"`
	Edges          []DraftGraphEdge `json:"edges"`
	Digest         string           `json:"digest"`
}

// draftGraphCanonical is the exact digest input. Field order is fixed by
// declaration order; slices are pre-sorted by the builder.
type draftGraphCanonical struct {
	WorkspaceID    string           `json:"workspace_id"`
	SourceID       string           `json:"source_id"`
	RootNativeID   string           `json:"root_native_id"`
	ConfigRevision int32            `json:"config_revision"`
	Nodes          []DraftGraphNode `json:"nodes"`
	Edges          []DraftGraphEdge `json:"edges"`
}

// BuildDraftGraph derives the non-executing draft closure of rootID from
// trusted read receipt observations. The closure follows only the root's
// outgoing predecessor dependencies, transitively; extra valid observations
// never expand it. Every visited node must carry a complete dependency
// observation (explicit [] for zero edges; legacy incomplete observations are
// never eligible), every dependency endpoint must have a receipt, only exact
// "blocks" edges are accepted, and cycles, malformed counts, duplicate or
// blank IDs/revisions, root revision mismatches, and size bounds are rejected.
func BuildDraftGraph(workspaceID, sourceID, rootID, expectedRootRevision string, configRevision int32, observations []GraphObservation) (DraftGraph, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return DraftGraph{}, fmt.Errorf("draft graph: blank workspace ID")
	}
	if strings.TrimSpace(sourceID) == "" {
		return DraftGraph{}, fmt.Errorf("draft graph: blank source ID")
	}
	if strings.TrimSpace(rootID) == "" {
		return DraftGraph{}, fmt.Errorf("draft graph: blank root native ID")
	}
	if strings.HasPrefix(rootID, "external:") {
		return DraftGraph{}, fmt.Errorf("draft graph: root native ID %q is external", rootID)
	}
	if strings.TrimSpace(expectedRootRevision) == "" {
		return DraftGraph{}, fmt.Errorf("draft graph: blank expected root revision")
	}
	if configRevision <= 0 {
		return DraftGraph{}, fmt.Errorf("draft graph: invalid config revision %d", configRevision)
	}
	if len(observations) > maxDraftGraphObservations {
		return DraftGraph{}, fmt.Errorf("draft graph: %d observations exceed bound %d", len(observations), maxDraftGraphObservations)
	}

	index := make(map[string]*GraphObservation, len(observations))
	for i := range observations {
		observation := &observations[i]
		if strings.TrimSpace(observation.Item.ID) == "" {
			return DraftGraph{}, fmt.Errorf("draft graph: observation %d missing native ID", i)
		}
		// External boundaries cannot become native nodes through a matching
		// receipt identity either: reject them for any root or visited node.
		if strings.HasPrefix(observation.Item.ID, "external:") {
			return DraftGraph{}, fmt.Errorf("draft graph: observation native ID %q is external", observation.Item.ID)
		}
		if strings.TrimSpace(observation.Item.Revision) == "" {
			return DraftGraph{}, fmt.Errorf("draft graph: observation %q missing revision", observation.Item.ID)
		}
		if strings.TrimSpace(observation.ReceiptID) == "" {
			return DraftGraph{}, fmt.Errorf("draft graph: observation %q missing receipt ID", observation.Item.ID)
		}
		if _, duplicate := index[observation.Item.ID]; duplicate {
			return DraftGraph{}, fmt.Errorf("draft graph: duplicate native ID %q", observation.Item.ID)
		}
		index[observation.Item.ID] = observation
	}

	root, ok := index[rootID]
	if !ok {
		return DraftGraph{}, fmt.Errorf("draft graph: root %q has no observation", rootID)
	}
	if root.Item.Revision != expectedRootRevision {
		return DraftGraph{}, fmt.Errorf("draft graph: root %q revision %q does not match expected %q", rootID, root.Item.Revision, expectedRootRevision)
	}

	nodes := make([]DraftGraphNode, 0, len(index))
	edges := make([]DraftGraphEdge, 0, len(index))
	seenEdges := make(map[DraftGraphEdge]bool)
	// 0 unvisited, 1 on the current predecessor chain, 2 fully closed.
	states := make(map[string]int8, len(index))
	var visit func(id string) error
	visit = func(id string) error {
		switch states[id] {
		case 1:
			return fmt.Errorf("draft graph: dependency cycle through %q", id)
		case 2:
			return nil
		}
		states[id] = 1
		observation := index[id]
		if !observation.Item.DependenciesComplete {
			return fmt.Errorf("draft graph: incomplete dependency observation for %q", id)
		}
		if observation.Item.Dependencies == nil {
			// Complete with null dependencies is malformed; zero edges
			// must be an explicit empty slice.
			return fmt.Errorf("draft graph: complete observation for %q carries null dependencies", id)
		}
		if observation.Item.DependencyCount != len(observation.Item.Dependencies) {
			return fmt.Errorf("draft graph: observation %q dependency count %d does not match %d observed edges", id, observation.Item.DependencyCount, len(observation.Item.Dependencies))
		}
		for _, dependency := range observation.Item.Dependencies {
			if dependency.DependencyType != draftGraphDependencyTypeBlocks {
				return fmt.Errorf("draft graph: unsupported edge kind %q on %q", dependency.DependencyType, id)
			}
			if strings.TrimSpace(dependency.ID) == "" {
				return fmt.Errorf("draft graph: observation %q has a dependency with a missing endpoint ID", id)
			}
			// Explicit external endpoints (the source's external: refs) are
			// never adopted as native producers, even when a receipt happens
			// to carry the same ID: fail regardless of receipt presence.
			if strings.HasPrefix(dependency.ID, "external:") {
				return fmt.Errorf("draft graph: dependency endpoint %q of %q is external", dependency.ID, id)
			}
			if _, ok := index[dependency.ID]; !ok {
				return fmt.Errorf("draft graph: dependency endpoint %q of %q has no receipt", dependency.ID, id)
			}
			if err := visit(dependency.ID); err != nil {
				return err
			}
			edge := DraftGraphEdge{PredecessorNativeID: dependency.ID, ConsumerNativeID: id, DependencyType: dependency.DependencyType}
			if seenEdges[edge] {
				return fmt.Errorf("draft graph: duplicate edge %q -> %q", edge.PredecessorNativeID, edge.ConsumerNativeID)
			}
			seenEdges[edge] = true
			edges = append(edges, edge)
			if len(edges) > maxDraftGraphEdges {
				return fmt.Errorf("draft graph: edges exceed bound %d", maxDraftGraphEdges)
			}
		}
		states[id] = 2
		nodes = append(nodes, DraftGraphNode{
			NativeID:   observation.Item.ID,
			Revision:   observation.Item.Revision,
			Title:      observation.Item.Title,
			Status:     observation.Item.Status,
			ReceiptID:  observation.ReceiptID,
			ObservedAt: observation.ObservedAt,
		})
		if len(nodes) > maxDraftGraphNodes {
			return fmt.Errorf("draft graph: nodes exceed bound %d", maxDraftGraphNodes)
		}
		return nil
	}
	if err := visit(rootID); err != nil {
		return DraftGraph{}, err
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NativeID < nodes[j].NativeID })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].PredecessorNativeID != edges[j].PredecessorNativeID {
			return edges[i].PredecessorNativeID < edges[j].PredecessorNativeID
		}
		if edges[i].ConsumerNativeID != edges[j].ConsumerNativeID {
			return edges[i].ConsumerNativeID < edges[j].ConsumerNativeID
		}
		return edges[i].DependencyType < edges[j].DependencyType
	})

	canonical := draftGraphCanonical{
		WorkspaceID:    workspaceID,
		SourceID:       sourceID,
		RootNativeID:   rootID,
		ConfigRevision: configRevision,
		Nodes:          nodes,
		Edges:          edges,
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return DraftGraph{}, fmt.Errorf("draft graph: encode canonical form: %w", err)
	}
	digest := sha256.Sum256(payload)

	graph := DraftGraph{
		WorkspaceID:    workspaceID,
		SourceID:       sourceID,
		RootNativeID:   rootID,
		ConfigRevision: configRevision,
		Nodes:          nodes,
		Edges:          edges,
		Digest:         hex.EncodeToString(digest[:]),
	}
	// The serialized bound covers the FINAL graph JSON including the digest
	// field, not just the canonical payload: the digest adds 76 bytes
	// (key/punctuation plus 64 hex characters).
	final, err := json.Marshal(graph)
	if err != nil {
		return DraftGraph{}, fmt.Errorf("draft graph: encode final form: %w", err)
	}
	if len(final) > maxDraftGraphSerializedBytes {
		return DraftGraph{}, fmt.Errorf("draft graph: serialized closure of %d bytes exceeds bound %d", len(final), maxDraftGraphSerializedBytes)
	}
	return graph, nil
}
