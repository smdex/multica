package service

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/beads"
)

// graphObs builds one complete read receipt observation.
func graphObs(id, revision string, deps []beads.Dependency) GraphObservation {
	return GraphObservation{
		ReceiptID:  "receipt-" + id,
		ObservedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Add(time.Hour),
		Item: beads.Issue{
			IssueSummary:          beads.IssueSummary{ID: id, Title: "title " + id, Status: "open", DependencyCount: len(deps)},
			Revision:              revision,
			Dependencies:          deps,
			DependenciesComplete:  true,
		},
	}
}

func blocks(ids ...string) []beads.Dependency {
	deps := make([]beads.Dependency, 0, len(ids))
	for _, id := range ids {
		deps = append(deps, beads.Dependency{ID: id, DependencyType: "blocks"})
	}
	return deps
}

// abCObservations returns the canonical A/B->C closure: root C blocked by A
// and B, both leaves with explicit zero-edge complete observations.
func abCObservations() []GraphObservation {
	return []GraphObservation{
		graphObs("bd-c", "rev-c", blocks("bd-a", "bd-b")),
		graphObs("bd-a", "rev-a", blocks()),
		graphObs("bd-b", "rev-b", blocks()),
	}
}

func buildAB(t *testing.T) DraftGraph {
	t.Helper()
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-c", 7, abCObservations())
	if err != nil {
		t.Fatalf("BuildDraftGraph: %v", err)
	}
	return graph
}

func TestBuildDraftGraphABC(t *testing.T) {
	graph := buildAB(t)
	observedAt := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Add(time.Hour)
	wantNodes := []DraftGraphNode{
		{NativeID: "bd-a", Revision: "rev-a", Title: "title bd-a", Status: "open", ReceiptID: "receipt-bd-a", ObservedAt: observedAt},
		{NativeID: "bd-b", Revision: "rev-b", Title: "title bd-b", Status: "open", ReceiptID: "receipt-bd-b", ObservedAt: observedAt},
		{NativeID: "bd-c", Revision: "rev-c", Title: "title bd-c", Status: "open", ReceiptID: "receipt-bd-c", ObservedAt: observedAt},
	}
	if !reflect.DeepEqual(graph.Nodes, wantNodes) {
		t.Errorf("nodes = %+v, want %+v", graph.Nodes, wantNodes)
	}
	wantEdges := []DraftGraphEdge{
		{PredecessorNativeID: "bd-a", ConsumerNativeID: "bd-c", DependencyType: "blocks"},
		{PredecessorNativeID: "bd-b", ConsumerNativeID: "bd-c", DependencyType: "blocks"},
	}
	if !reflect.DeepEqual(graph.Edges, wantEdges) {
		t.Errorf("edges = %+v, want %+v", graph.Edges, wantEdges)
	}
	if graph.WorkspaceID != "ws-1" || graph.SourceID != "src-1" || graph.RootNativeID != "bd-c" || graph.ConfigRevision != 7 {
		t.Errorf("scope fields = %+v", graph)
	}
	if len(graph.Digest) != 64 {
		t.Errorf("digest %q is not 64 hex characters", graph.Digest)
	}
}

func TestBuildDraftGraphRejects(t *testing.T) {
	cases := map[string][]GraphObservation{
		"cycle": {
			graphObs("bd-a", "rev-a", blocks("bd-b")),
			graphObs("bd-b", "rev-b", blocks("bd-a")),
		},
		"self cycle": {
			graphObs("bd-a", "rev-a", blocks("bd-a")),
		},
		"missing receipt for dependency endpoint": {
			graphObs("bd-a", "rev-a", blocks("bd-x")),
		},
		"external endpoint without receipt": {
			graphObs("bd-a", "rev-a", blocks("ext:other-1")),
		},
		"external endpoint rejected even with receipt": {
			graphObs("bd-a", "rev-a", blocks("external:crm:pod-client")),
			graphObs("external:crm:pod-client", "rev-ext", blocks()),
		},
		"incomplete dependency observation": {
			{ReceiptID: "r", Item: beads.Issue{IssueSummary: beads.IssueSummary{ID: "bd-a"}, Revision: "rev-a"}},
		},
		"complete with null dependencies": {
			{ReceiptID: "r", Item: beads.Issue{IssueSummary: beads.IssueSummary{ID: "bd-a"}, Revision: "rev-a", DependenciesComplete: true}},
		},
		"unsupported edge kind": {
			graphObs("bd-a", "rev-a", []beads.Dependency{{ID: "bd-b", DependencyType: "relates-to"}}),
			graphObs("bd-b", "rev-b", blocks()),
		},
		"duplicate native ID": {
			graphObs("bd-a", "rev-a", blocks()),
			graphObs("bd-a", "rev-a2", blocks()),
		},
		"missing native ID": {
			graphObs("", "rev-a", blocks()),
		},
		"missing revision": {
			graphObs("bd-a", "", blocks()),
		},
		"missing receipt ID": {
			{Item: graphObs("bd-a", "rev-a", blocks()).Item},
		},
		"missing dependency endpoint ID": {
			graphObs("bd-a", "rev-a", []beads.Dependency{{ID: " ", DependencyType: "blocks"}}),
		},
	}
	for name, observations := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 1, observations); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
	t.Run("malformed dependency count", func(t *testing.T) {
		observation := graphObs("bd-a", "rev-a", blocks("bd-b", "bd-c"))
		observation.Item.DependencyCount = 3
		if _, err := BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 1, []GraphObservation{observation}); err == nil {
			t.Fatal("expected error for malformed dependency count")
		}
	})
	t.Run("root revision mismatch", func(t *testing.T) {
		if _, err := BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-stale", 1, abCObservations()); err == nil {
			t.Fatal("expected error for root revision mismatch")
		}
	})
	t.Run("external root rejected even with receipt", func(t *testing.T) {
		observations := append(abCObservations(), graphObs("external:crm:pod-client", "rev-ext", blocks("bd-a")))
		if _, err := BuildDraftGraph("ws-1", "src-1", "external:crm:pod-client", "rev-ext", 1, observations); err == nil {
			t.Fatal("expected error for external root")
		}
	})
	t.Run("external visited node rejected even with receipt", func(t *testing.T) {
		// Root is native, but one visited dependency endpoint is external
		// and carries a receipt: still rejected, never adopted as a node.
		observations := []GraphObservation{
			graphObs("bd-a", "rev-a", blocks("external:crm:pod-client")),
			graphObs("external:crm:pod-client", "rev-ext", blocks()),
		}
		if _, err := BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 1, observations); err == nil {
			t.Fatal("expected error for external visited node")
		}
	})
	t.Run("root without observation", func(t *testing.T) {
		if _, err := BuildDraftGraph("ws-1", "src-1", "bd-z", "rev-a", 1, abCObservations()); err == nil {
			t.Fatal("expected error for unknown root")
		}
	})
	t.Run("blank scope inputs", func(t *testing.T) {
		for _, args := range [][4]string{{"", "src-1", "bd-a", "rev-a"}, {"ws-1", "", "bd-a", "rev-a"}, {"ws-1", "src-1", "", "rev-a"}, {"ws-1", "src-1", "bd-a", ""}} {
			if _, err := BuildDraftGraph(args[0], args[1], args[2], args[3], 1, abCObservations()); err == nil {
				t.Fatalf("expected error for blank input %+v", args)
			}
		}
	})
}

func TestBuildDraftGraphObservationBound(t *testing.T) {
	observations := make([]GraphObservation, maxDraftGraphObservations+1)
	for i := range observations {
		observations[i] = graphObs(fmt.Sprintf("bd-%03d", i), "rev", blocks())
	}
	if _, err := BuildDraftGraph("ws-1", "src-1", "bd-000", "rev", 1, observations); err == nil {
		t.Fatal("expected error for observation bound")
	}
	observations = observations[:maxDraftGraphObservations]
	if _, err := BuildDraftGraph("ws-1", "src-1", "bd-000", "rev", 1, observations); err != nil {
		t.Fatalf("observations at bound: %v", err)
	}
}

func TestBuildDraftGraphNodeBound(t *testing.T) {
	// A chain bd-000 -> bd-001 -> ... -> bd-128 has 129 nodes and exceeds
	// the closure node bound.
	chain := make([]GraphObservation, 0, maxDraftGraphNodes+1)
	for i := 0; i <= maxDraftGraphNodes; i++ {
		id := fmt.Sprintf("bd-%03d", i)
		var deps []beads.Dependency
		if i < maxDraftGraphNodes {
			deps = blocks(fmt.Sprintf("bd-%03d", i+1))
		}
		chain = append(chain, graphObs(id, "rev", deps))
	}
	if _, err := BuildDraftGraph("ws-1", "src-1", "bd-000", "rev", 1, chain); err == nil {
		t.Fatal("expected error for node bound")
	}
	// Dropping the last link keeps the closure at exactly 128 nodes.
	chain = chain[:maxDraftGraphNodes]
	chain[maxDraftGraphNodes-1].Item.Dependencies = blocks()
	chain[maxDraftGraphNodes-1].Item.DependencyCount = 0
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-000", "rev", 1, chain)
	if err != nil {
		t.Fatalf("nodes at bound: %v", err)
	}
	if len(graph.Nodes) != maxDraftGraphNodes {
		t.Fatalf("nodes = %d, want %d", len(graph.Nodes), maxDraftGraphNodes)
	}
}

func TestBuildDraftGraphEdgeBound(t *testing.T) {
	// Dependencies are predecessors, so the root lists the non-root
	// consumers plus all 32 predecessors; each non-root consumer also lists
	// the 32 predecessors. 16 non-root consumers yields 32+33*16 = 560
	// closure edges with only 49 nodes: the edge bound fires while nodes
	// stay well in bound.
	predecessors := 32
	predDeps := func() []beads.Dependency {
		deps := make([]beads.Dependency, 0, predecessors)
		for i := 0; i < predecessors; i++ {
			deps = append(deps, beads.Dependency{ID: fmt.Sprintf("bd-p%02d", i), DependencyType: "blocks"})
		}
		return deps
	}
	build := func(consumers int) []GraphObservation {
		rootDeps := predDeps()
		for i := 1; i <= consumers; i++ {
			rootDeps = append(rootDeps, beads.Dependency{ID: fmt.Sprintf("bd-c%02d", i), DependencyType: "blocks"})
		}
		observations := []GraphObservation{graphObs("bd-c00", "rev", rootDeps)}
		for i := 1; i <= consumers; i++ {
			observations = append(observations, graphObs(fmt.Sprintf("bd-c%02d", i), "rev", predDeps()))
		}
		for i := 0; i < predecessors; i++ {
			observations = append(observations, graphObs(fmt.Sprintf("bd-p%02d", i), "rev", blocks()))
		}
		return observations
	}
	if _, err := BuildDraftGraph("ws-1", "src-1", "bd-c00", "rev", 1, build(16)); err == nil {
		t.Fatal("expected error for edge bound")
	}
	// 14 non-root consumers leaves 32+33*14 = 494 edges, in bound.
	if _, err := BuildDraftGraph("ws-1", "src-1", "bd-c00", "rev", 1, build(14)); err != nil {
		t.Fatalf("edges under bound: %v", err)
	}
}

func TestBuildDraftGraphConfigRevisionGuard(t *testing.T) {
	for _, configRevision := range []int32{0, -1} {
		if _, err := BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", configRevision, []GraphObservation{graphObs("bd-a", "rev-a", blocks())}); err == nil {
			t.Fatalf("expected error for config revision %d", configRevision)
		}
	}
}

func TestBuildDraftGraphSerializedBoundExactBoundary(t *testing.T) {
	// The serialized bound covers the FINAL graph JSON including the
	// digest field (76 extra bytes vs the canonical payload). Tune the
	// root node title so the marshaled graph is exactly 2 MiB, then +1.
	base := graphObs("bd-a", "rev-a", blocks())
	base.Item.Title = strings.Repeat("x", 16)
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 1, []GraphObservation{base})
	if err != nil {
		t.Fatalf("baseline build: %v", err)
	}
	baseline, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	pad := maxDraftGraphSerializedBytes - len(baseline)
	if pad < 0 {
		t.Fatalf("baseline graph already exceeds bound: %d bytes", len(baseline))
	}
	// 'x' marshals one byte per rune, so growing the title by pad grows
	// the final JSON by exactly pad bytes.
	exact := graphObs("bd-a", "rev-a", blocks())
	exact.Item.Title = strings.Repeat("x", 16+pad)
	graph, err = BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 1, []GraphObservation{exact})
	if err != nil {
		t.Fatalf("exact boundary build: %v", err)
	}
	final, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal exact boundary: %v", err)
	}
	if len(final) != maxDraftGraphSerializedBytes {
		t.Fatalf("final serialized size = %d, want exactly %d", len(final), maxDraftGraphSerializedBytes)
	}
	if len(graph.Digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex characters", graph.Digest)
	}
	over := graphObs("bd-a", "rev-a", blocks())
	over.Item.Title = strings.Repeat("x", 16+pad+1)
	if _, err := BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 1, []GraphObservation{over}); err == nil {
		t.Fatal("expected error one byte over the serialized bound")
	}
}

func TestBuildDraftGraphPermutationDigestEqual(t *testing.T) {
	base := buildAB(t)
	permuted := []GraphObservation{abCObservations()[2], abCObservations()[0], abCObservations()[1]}
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-c", 7, permuted)
	if err != nil {
		t.Fatalf("BuildDraftGraph: %v", err)
	}
	if graph.Digest != base.Digest {
		t.Errorf("permuted digest %s != base %s", graph.Digest, base.Digest)
	}
}

func TestBuildDraftGraphTopologyChangeAltersDigest(t *testing.T) {
	base := buildAB(t)
	// Same item revisions, titles, statuses: only the topology changes
	// (leaf B gains a dependency on A).
	observations := abCObservations()
	observations[2] = graphObs("bd-b", "rev-b", blocks("bd-a"))
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-c", 7, observations)
	if err != nil {
		t.Fatalf("BuildDraftGraph: %v", err)
	}
	if graph.Digest == base.Digest {
		t.Error("digest must change when topology changes with identical item revisions")
	}
}

func TestBuildDraftGraphDigestCoversScopeConfigRoot(t *testing.T) {
	base := buildAB(t)
	checks := map[string]func() (DraftGraph, error){
		"workspace":     func() (DraftGraph, error) { return BuildDraftGraph("ws-2", "src-1", "bd-c", "rev-c", 7, abCObservations()) },
		"source":        func() (DraftGraph, error) { return BuildDraftGraph("ws-1", "src-2", "bd-c", "rev-c", 7, abCObservations()) },
		"config rev":    func() (DraftGraph, error) { return BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-c", 8, abCObservations()) },
		"root":          func() (DraftGraph, error) { return BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 7, abCObservations()) },
	}
	for name, build := range checks {
		graph, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if graph.Digest == base.Digest {
			t.Errorf("digest must cover %s", name)
		}
	}
	// A changed leaf revision (provenance beyond topology) changes the digest.
	observations := abCObservations()
	observations[1].Item.Revision = "rev-b2"
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-c", 7, observations)
	if err != nil {
		t.Fatalf("revision change: %v", err)
	}
	if graph.Digest == base.Digest {
		t.Error("digest must cover item revisions")
	}
	// A different receipt provenance for the same item changes the digest.
	observations = abCObservations()
	observations[0].ReceiptID = "receipt-bd-c-other"
	graph, err = BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-c", 7, observations)
	if err != nil {
		t.Fatalf("provenance change: %v", err)
	}
	if graph.Digest == base.Digest {
		t.Error("digest must cover receipt provenance")
	}
}

func TestBuildDraftGraphExtraReceiptDoesNotExpandClosure(t *testing.T) {
	base := buildAB(t)
	observations := append(abCObservations(),
		graphObs("bd-extra", "rev-extra", blocks("bd-a")),
		graphObs("bd-extra-2", "rev-extra-2", blocks("bd-extra")),
	)
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-c", "rev-c", 7, observations)
	if err != nil {
		t.Fatalf("BuildDraftGraph: %v", err)
	}
	for _, node := range graph.Nodes {
		if strings.HasPrefix(node.NativeID, "bd-extra") {
			t.Errorf("extra receipt %q expanded the closure", node.NativeID)
		}
	}
	if len(graph.Nodes) != len(base.Nodes) || len(graph.Edges) != len(base.Edges) {
		t.Errorf("closure size = %d nodes/%d edges, want %d/%d", len(graph.Nodes), len(graph.Edges), len(base.Nodes), len(base.Edges))
	}
	if graph.Digest != base.Digest {
		t.Error("irrelevant extra receipts must not change the digest")
	}
}

func TestBuildDraftGraphRetainsOpaqueValuesExactly(t *testing.T) {
	id := "bd-1a2b Ó"
	revision := " rev/with spaces\t"
	observations := []GraphObservation{graphObs(id, revision, blocks())}
	observations[0].Item.Title = "títle ✅"
	graph, err := BuildDraftGraph("ws-1", "src-1", id, revision, 1, observations)
	if err != nil {
		t.Fatalf("BuildDraftGraph: %v", err)
	}
	node := graph.Nodes[0]
	if node.NativeID != id || node.Revision != revision || node.Title != "títle ✅" {
		t.Errorf("opaque values not retained verbatim: %+v", node)
	}
}

func TestBuildDraftGraphZeroEdgeRootExplicitComplete(t *testing.T) {
	graph, err := BuildDraftGraph("ws-1", "src-1", "bd-a", "rev-a", 1, []GraphObservation{graphObs("bd-a", "rev-a", blocks())})
	if err != nil {
		t.Fatalf("BuildDraftGraph: %v", err)
	}
	if len(graph.Nodes) != 1 || len(graph.Edges) != 0 {
		t.Fatalf("nodes = %d, edges = %d; want 1 node, 0 edges", len(graph.Nodes), len(graph.Edges))
	}
}

func TestBuildDraftGraphDeterministicSortedOutput(t *testing.T) {
	graph := buildAB(t)
	for i := 1; i < len(graph.Nodes); i++ {
		if graph.Nodes[i-1].NativeID >= graph.Nodes[i].NativeID {
			t.Errorf("nodes not sorted by native ID: %q >= %q", graph.Nodes[i-1].NativeID, graph.Nodes[i].NativeID)
		}
	}
	for i := 1; i < len(graph.Edges); i++ {
		prev, cur := graph.Edges[i-1], graph.Edges[i]
		if prev.PredecessorNativeID > cur.PredecessorNativeID ||
			(prev.PredecessorNativeID == cur.PredecessorNativeID && prev.ConsumerNativeID >= cur.ConsumerNativeID) {
			t.Errorf("edges not sorted: %+v then %+v", prev, cur)
		}
	}
}
