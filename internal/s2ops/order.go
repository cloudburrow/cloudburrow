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

import (
	"math"
	"sort"

	"github.com/golang/geo/s2"
)

// noInput marks a graph edge with no input edge id (the sibling of an
// undirected edge), sorting after every labelled edge.
const noInput = math.MaxInt

// Edge is a directed edge with the id of the input edge it came from;
// input ids order output loops and polylines as S2Builder's do.
type Edge struct {
	A, B s2.Point
	ID   int
}

// vertexLess orders points as S2Builder::SortInputVertices does: by leaf
// cell id, then by coordinates.
func vertexLess(a, b s2.Point) bool {
	ca, cb := s2.CellIDFromLatLng(s2.LatLngFromPoint(a)), s2.CellIDFromLatLng(s2.LatLngFromPoint(b))
	if ca != cb {
		return ca < cb
	}
	return pointLess(a, b)
}

func pointLess(a, b s2.Point) bool {
	if a.X != b.X {
		return a.X < b.X
	}
	if a.Y != b.Y {
		return a.Y < b.Y
	}
	return a.Z < b.Z
}

// SortVertices sorts points in S2Builder's vertex order.
func SortVertices(ps []s2.Point) {
	sort.SliceStable(ps, func(i, j int) bool { return vertexLess(ps[i], ps[j]) })
}

// canonicalizeLoopOrder rotates a loop's edges, given their min input ids,
// so that the edge(s) with the highest input id come last
// (Graph::CanonicalizeLoopOrder).
func canonicalizeLoopOrder(ids []int) int {
	if len(ids) == 0 {
		return 0
	}
	pos := 0
	sawGap := false
	for i := 1; i < len(ids); i++ {
		cmp := 0
		if ids[i] < ids[pos] {
			cmp = -1
		} else if ids[i] > ids[pos] {
			cmp = 1
		}
		if cmp < 0 {
			sawGap = true
		} else if cmp > 0 || !sawGap {
			pos = i
			sawGap = false
		}
	}
	pos++
	if pos == len(ids) {
		pos = 0
	}
	return pos
}

func rotateEdges(es []Edge) []Edge {
	ids := make([]int, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	pos := canonicalizeLoopOrder(ids)
	return append(append([]Edge(nil), es[pos:]...), es[:pos]...)
}

// graph is the vertex numbering of a set of edges: ids in S2Builder's
// vertex order.
type graph struct {
	pts []s2.Point
	id  map[s2.Point]int
}

func newGraph(points []s2.Point) *graph {
	g := &graph{id: map[s2.Point]int{}}
	for _, p := range points {
		if _, ok := g.id[p]; !ok {
			g.id[p] = -1
			g.pts = append(g.pts, p)
		}
	}
	SortVertices(g.pts)
	for i, p := range g.pts {
		g.id[p] = i
	}
	return g
}

// Polylines assembles undirected edges into polylines as S2Builder's
// PolylineBuilder::BuildPaths does for undirected edges: duplicate edges
// merged, paths broken at every vertex whose degree is not two, started in
// input edge order, loops canonicalized, and the polylines sorted by their
// first edge's input id. Degenerate edges are dropped.
func Polylines(in []Edge) [][]s2.Point {
	type key struct{ a, b s2.Point }
	und := map[key]int{} // undirected edge -> index in labelled
	var labelled []Edge
	var pts []s2.Point
	for _, e := range in {
		if e.A == e.B {
			continue
		}
		k := key{e.A, e.B}
		if pointLess(e.B, e.A) {
			k = key{e.B, e.A}
		}
		if i, ok := und[k]; ok {
			if e.ID < labelled[i].ID {
				labelled[i].ID = e.ID
			}
			continue
		}
		und[k] = len(labelled)
		labelled = append(labelled, e)
		pts = append(pts, e.A, e.B)
	}
	if len(labelled) == 0 {
		return nil
	}
	g := newGraph(pts)
	type gedge struct {
		src, dst int
		id       int
		sib      int
	}
	var es []gedge
	for _, e := range labelled {
		es = append(es, gedge{src: g.id[e.A], dst: g.id[e.B], id: e.ID}, gedge{src: g.id[e.B], dst: g.id[e.A], id: noInput})
	}
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].src != es[j].src {
			return es[i].src < es[j].src
		}
		if es[i].dst != es[j].dst {
			return es[i].dst < es[j].dst
		}
		return es[i].id < es[j].id
	})
	out := map[int][]int{}
	for i := range es {
		out[es[i].src] = append(out[es[i].src], i)
	}
	for i := range es {
		for _, j := range out[es[i].dst] {
			if es[j].dst == es[i].src && es[j].id != es[i].id && (es[i].id == noInput) != (es[j].id == noInput) {
				es[i].sib = j
			}
		}
	}
	// a sibling's min input id is noInput; the labelled edge's is its own.
	order := make([]int, len(es))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		if es[order[a]].id != es[order[b]].id {
			return es[order[a]].id < es[order[b]].id
		}
		return order[a] < order[b]
	})
	used := make([]bool, len(es))
	left := len(labelled)
	interior := func(v int) bool { return len(out[v]) == 2 }
	buildPath := func(e int) []int {
		var path []int
		start := es[e].src
		for {
			path = append(path, e)
			used[e], used[es[e].sib] = true, true
			left--
			v := es[e].dst
			if !interior(v) || v == start {
				break
			}
			for _, e2 := range out[v] {
				if !used[e2] {
					e = e2
				}
			}
		}
		return path
	}
	var paths [][]int
	for _, e := range order {
		if !used[e] && !interior(es[e].src) {
			paths = append(paths, buildPath(e))
		}
	}
	for _, e := range order {
		if left == 0 {
			break
		}
		if used[e] {
			continue
		}
		p := buildPath(e)
		ids := make([]int, len(p))
		for i, x := range p {
			ids[i] = minID(es[x].id, es[es[x].sib].id)
		}
		pos := canonicalizeLoopOrder(ids)
		paths = append(paths, append(append([]int(nil), p[pos:]...), p[:pos]...))
	}
	// Edges' min input ids: S2 labels only the input direction.
	sort.SliceStable(paths, func(a, b int) bool {
		ia, ib := es[paths[a][0]].id, es[paths[b][0]].id
		if ia != ib {
			return ia < ib
		}
		return paths[a][0] < paths[b][0]
	})
	var res [][]s2.Point
	for _, p := range paths {
		vs := []s2.Point{g.pts[es[p[0]].src]}
		for _, e := range p {
			vs = append(vs, g.pts[es[e].dst])
		}
		res = append(res, vs)
	}
	return res
}

func minID(a, b int) int {
	if a < b {
		return a
	}
	return b
}
