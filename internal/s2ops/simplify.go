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
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// MaxSnapRadius is S2Builder::SnapFunction::kMaxSnapRadius (70 degrees).
const MaxSnapRadius = s1.Angle(70 * 3.141592653589793 / 180)

// Simplified is a simplified geometry: points, polylines, and polygon
// loops with the interior on their left.
type Simplified struct {
	Points []s2.Point
	Lines  [][]s2.Point
	Loops  [][]s2.Point
}

// Simplify simplifies points, polylines and a polygon's oriented loops with
// snap radius r as S2Builder does with an IdentitySnapFunction(r) and
// simplify_edge_chains: vertices are snapped to sites chosen in vertex order
// at least r apart, then chains of vertices that belong to one chain only
// are replaced, greedily, by single edges passing within r of every removed
// vertex. Loops that collapse to sibling edge pairs become polylines, and
// geometry that collapses to one vertex becomes a point.
func Simplify(r s1.Angle, points []s2.Point, lines [][]s2.Point, loops [][]s2.Point) Simplified {
	var all []s2.Point
	all = append(all, points...)
	for _, l := range lines {
		all = append(all, l...)
	}
	for _, l := range loops {
		all = append(all, l...)
	}
	sorted := append([]s2.Point(nil), all...)
	SortVertices(sorted)
	var sites []s2.Point
	for _, v := range sorted {
		add := true
		for _, s := range sites {
			if v.Distance(s) <= r {
				add = false
				break
			}
		}
		if add {
			sites = append(sites, v)
		}
	}
	snap := func(v s2.Point) s2.Point {
		best, bd := v, s1.Angle(-1)
		for _, s := range sites {
			if d := v.Distance(s); bd < 0 || d < bd {
				best, bd = s, d
			}
		}
		return best
	}
	order := newGraph(sites)
	type chain struct {
		vs   []s2.Point // snapped, consecutive duplicates dropped
		orig [][]s2.Point
		ring bool
		id   int
	}
	var chains []chain
	var out Simplified
	id := len(points)
	// incidence counts edge ends at each site, to find interior vertices.
	inc := map[s2.Point]int{}
	for _, p := range points {
		inc[snap(p)] += 2 // a point pins its site
	}
	mk := func(l []s2.Point, ring bool) {
		c := chain{ring: ring, id: id}
		id += len(l)
		for _, v := range l {
			s := snap(v)
			if n := len(c.vs); n > 0 && c.vs[n-1] == s {
				c.orig[n-1] = append(c.orig[n-1], v)
				continue
			}
			c.vs = append(c.vs, s)
			c.orig = append(c.orig, []s2.Point{v})
		}
		if ring && len(c.vs) > 1 && c.vs[0] == c.vs[len(c.vs)-1] {
			c.orig[0] = append(c.orig[0], c.orig[len(c.vs)-1]...)
			c.vs, c.orig = c.vs[:len(c.vs)-1], c.orig[:len(c.vs)-1]
		}
		chains = append(chains, c)
		n := len(c.vs)
		switch {
		case n == 1:
			inc[c.vs[0]] += 2
		case ring:
			for _, v := range c.vs {
				inc[v] += 2
			}
		default:
			for i, v := range c.vs {
				if i == 0 || i == n-1 {
					inc[v] += 3 // endpoints stay
				} else {
					inc[v] += 2
				}
			}
		}
	}
	for _, l := range lines {
		mk(l, false)
	}
	for _, l := range loops {
		mk(l, true)
	}
	// removable reports whether chain vertex k may go, the new edge a-b
	// passing within r of the vertices snapped to it.
	removable := func(c chain, k int, a, b s2.Point) bool {
		if inc[c.vs[k]] != 2 || a == b {
			return false
		}
		for _, o := range append([]s2.Point{c.vs[k]}, c.orig[k]...) {
			if s2.DistanceFromSegment(o, a, b) > r {
				return false
			}
		}
		return true
	}
	simplifyRun := func(c chain, idx []int) []int {
		// idx are positions in c.vs; the first and last stay.
		keep := []int{idx[0]}
		i := 0
		for i < len(idx)-1 {
			j := i + 1
			for j+1 < len(idx) {
				ok := true
				for k := i + 1; k <= j; k++ {
					if !removable(c, idx[k], c.vs[idx[i]], c.vs[idx[j+1]]) {
						ok = false
						break
					}
				}
				if !ok {
					break
				}
				j++
			}
			keep = append(keep, idx[j])
			i = j
		}
		return keep
	}
	var lineEdges []Edge
	var ringEdges []Edge
	for _, c := range chains {
		n := len(c.vs)
		if n == 1 {
			out.Points = append(out.Points, c.vs[0])
			continue
		}
		if !c.ring {
			idx := make([]int, n)
			for i := range idx {
				idx[i] = i
			}
			keep := simplifyRun(c, idx)
			for i := 1; i < len(keep); i++ {
				lineEdges = append(lineEdges, Edge{c.vs[keep[i-1]], c.vs[keep[i]], c.id + keep[i-1]})
			}
			continue
		}
		// a ring: start at its first pinned vertex, or, when every vertex
		// may go, at the lowest in vertex order.
		start := -1
		for i, v := range c.vs {
			if inc[v] != 2 {
				start = i
				break
			}
		}
		if start < 0 {
			start = 0
			for i, v := range c.vs {
				if order.id[v] < order.id[c.vs[start]] {
					start = i
				}
			}
		}
		idx := []int{}
		for i := 0; i <= n; i++ {
			idx = append(idx, (start+i)%n)
		}
		// split the ring at its pinned vertices into runs.
		var keep []int
		run := []int{idx[0]}
		for _, k := range idx[1:] {
			run = append(run, k)
			if inc[c.vs[k]] != 2 || k == idx[0] {
				kr := simplifyRun(c, run)
				if len(keep) > 0 {
					kr = kr[1:]
				}
				keep = append(keep, kr...)
				run = []int{k}
			}
		}
		for i := 1; i < len(keep); i++ {
			ringEdges = append(ringEdges, Edge{c.vs[keep[i-1]], c.vs[keep[i]], c.id + keep[i-1]})
		}
	}
	// sibling pairs of ring edges collapse to polyline edges.
	type key struct{ a, b s2.Point }
	cnt := map[key][]int{}
	for i, e := range ringEdges {
		cnt[key{e.A, e.B}] = append(cnt[key{e.A, e.B}], i)
	}
	drop := make([]bool, len(ringEdges))
	for i, e := range ringEdges {
		if drop[i] {
			continue
		}
		rev := cnt[key{e.B, e.A}]
		for _, j := range rev {
			if !drop[j] && j != i {
				drop[i], drop[j] = true, true
				lineEdges = append(lineEdges, Edge{e.A, e.B, minID(e.ID, ringEdges[j].ID)})
				break
			}
		}
	}
	var keepRing []Edge
	for i, e := range ringEdges {
		if !drop[i] {
			keepRing = append(keepRing, e)
		}
	}
	if len(keepRing) > 0 {
		out.Loops = assembleLoops(keepRing)
	}
	out.Lines = Polylines(lineEdges)
	// points: snapped input points, and collapsed chains, once each, unless
	// a line or loop passes through them.
	covered := map[s2.Point]bool{}
	for _, l := range out.Lines {
		for _, v := range l {
			covered[v] = true
		}
	}
	for _, l := range out.Loops {
		for _, v := range l {
			covered[v] = true
		}
	}
	var pts []s2.Point
	for _, p := range points {
		pts = append(pts, snap(p))
	}
	pts = append(pts, out.Points...)
	out.Points = nil
	for _, p := range pts {
		if !covered[p] {
			covered[p] = true
			out.Points = append(out.Points, p)
		}
	}
	SortVertices(out.Points)
	return out
}
