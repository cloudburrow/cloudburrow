// Copyright 2016 Google Inc. All Rights Reserved.
// Copyright 2026 The CloudBurrow Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS-IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package s2ops

// Types shared by the S2Builder port (builder.go), its Graph
// (builder_graph.go), its layers (builder_layers.go) and S2BooleanOperation
// (boolean_operation.go), from src/s2/s2builder.h, src/s2/s2builder_graph.h
// and src/s2/id_set_lexicon.h.

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// The id types of s2builder.h.
type (
	InputEdgeID      = int32
	InputVertexID    = int32
	VertexID         = int32
	EdgeID           = int32
	InputEdgeIDSetID = int32
	Label            = int32
	LabelSetID       = int32
	SiteID           = int32
)

// GraphEdge is S2Builder::Graph::Edge, a pair of vertex ids.
type GraphEdge struct {
	First, Second VertexID
}

// Reverse is Graph::reverse(e).
func (e GraphEdge) Reverse() GraphEdge { return GraphEdge{e.Second, e.First} }

// edgeLess is std::pair's operator<.
func edgeLess(a, b GraphEdge) bool {
	if a.First != b.First {
		return a.First < b.First
	}
	return a.Second < b.Second
}

// EdgeType is S2Builder::EdgeType.
type EdgeType int

const (
	EdgeTypeDirected EdgeType = iota
	EdgeTypeUndirected
)

// DegenerateEdges is S2Builder::GraphOptions::DegenerateEdges.
type DegenerateEdges int

const (
	DegenerateEdgesDiscard DegenerateEdges = iota
	DegenerateEdgesDiscardExcess
	DegenerateEdgesKeep
)

// DuplicateEdges is S2Builder::GraphOptions::DuplicateEdges.
type DuplicateEdges int

const (
	DuplicateEdgesMerge DuplicateEdges = iota
	DuplicateEdgesKeep
)

// SiblingPairs is S2Builder::GraphOptions::SiblingPairs.
type SiblingPairs int

const (
	SiblingPairsDiscard SiblingPairs = iota
	SiblingPairsDiscardExcess
	SiblingPairsKeep
	SiblingPairsRequire
	SiblingPairsCreate
)

// GraphOptions is S2Builder::GraphOptions. Its zero value is not the C++
// default; use DefaultGraphOptions.
type GraphOptions struct {
	EdgeType             EdgeType
	DegenerateEdges      DegenerateEdges
	DuplicateEdges       DuplicateEdges
	SiblingPairs         SiblingPairs
	AllowVertexFiltering bool
}

// DefaultGraphOptions is GraphOptions()'s default: directed, keep,
// keep, keep, allow_vertex_filtering.
func DefaultGraphOptions() GraphOptions {
	return GraphOptions{EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep, true}
}

// Layer is S2Builder::Layer.
type Layer interface {
	GraphOptions() GraphOptions
	// Build assembles the layer's output from g, returning S2Error as a Go
	// error.
	Build(g *Graph) error
}

// IsFullPolygonPredicate is S2Builder::IsFullPolygonPredicate: whether a
// graph with no edges is the full polygon.
type IsFullPolygonPredicate func(g *Graph) (bool, error)

// IsFullPolygonUnspecified is S2Builder::IsFullPolygonUnspecified.
func IsFullPolygonUnspecified(g *Graph) (bool, error) {
	return false, errBuilder("A degenerate polygon was found, but no predicate was specified to determine whether the polygon is empty or full.  Call S2Builder::AddIsFullPolygonPredicate() to fix this problem.")
}

// IsFullPolygon is S2Builder::IsFullPolygon(is_full).
func IsFullPolygon(full bool) IsFullPolygonPredicate {
	return func(*Graph) (bool, error) { return full, nil }
}

// BuilderError is S2Error as raised by S2Builder and its layers.
type BuilderError struct{ Text string }

func (e *BuilderError) Error() string { return e.Text }

func errBuilder(text string) error { return &BuilderError{text} }

// emptySetID is IdSetLexicon::kEmptySetId.
const emptySetID = int32(math.MinInt32)

// IDSetLexicon is IdSetLexicon: sets of non-negative ids, empty and
// singleton sets held implicitly.
type IDSetLexicon struct {
	values [][]int32
	index  map[string]int32
}

// NewIDSetLexicon returns an empty lexicon.
func NewIDSetLexicon() *IDSetLexicon { return &IDSetLexicon{index: map[string]int32{}} }

// Add returns the id of the set of ids (sorted and deduplicated).
func (l *IDSetLexicon) Add(ids ...int32) int32 {
	if len(ids) == 0 {
		return emptySetID
	}
	if len(ids) == 1 {
		return ids[0]
	}
	set := slices.Clone(ids)
	slices.Sort(set)
	set = slices.Compact(set)
	if len(set) == 1 {
		return set[0]
	}
	var sb strings.Builder
	for _, v := range set {
		sb.WriteString(strconv.Itoa(int(v)))
		sb.WriteByte(',')
	}
	k := sb.String()
	if id, ok := l.index[k]; ok {
		return ^id
	}
	id := int32(len(l.values))
	l.values = append(l.values, set)
	l.index[k] = id
	return ^id
}

// IDSet returns the ids of a set id returned by Add.
func (l *IDSetLexicon) IDSet(setID int32) []int32 {
	if setID >= 0 {
		return []int32{setID}
	}
	if setID == emptySetID {
		return nil
	}
	return l.values[^setID]
}

// Clear empties the lexicon.
func (l *IDSetLexicon) Clear() {
	l.values = nil
	l.index = map[string]int32{}
}

// pointLess is S2Point's operator< (lexicographic on x, y, z).
func pointLess(a, b s2.Point) bool {
	if a.X != b.X {
		return a.X < b.X
	}
	if a.Y != b.Y {
		return a.Y < b.Y
	}
	return a.Z < b.Z
}

// Graph is S2Builder::Graph (its methods are in builder_graph.go).
type Graph struct {
	options                GraphOptions
	numVertices            int
	vertices               []s2.Point
	edges                  []GraphEdge
	inputEdgeIDSetIDs      []InputEdgeIDSetID
	inputEdgeIDSetLexicon  *IDSetLexicon
	labelSetIDs            []LabelSetID
	labelSetLexicon        *IDSetLexicon
	isFullPolygonPredicate IsFullPolygonPredicate
}

// MaxSnapRadius is S2Builder::SnapFunction::kMaxSnapRadius (70 degrees).
const MaxSnapRadius = s1.Angle(70 * math.Pi / 180)
