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
	"errors"
	"sort"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// mergeDist is how close two computed vertices must be to be one vertex:
// about 0.6 micrometres on the Earth, far below GoogleSQL's 15 printed
// digits and above the error of a computed crossing.
const mergeDist = s1.Angle(1e-13)

// snapper merges vertices closer than mergeDist, preferring the first seen
// (input vertices are added before computed crossings).
type snapper struct {
	exact map[s2.Point]s2.Point
	reps  []s2.Point
}

func newSnapper() *snapper { return &snapper{exact: map[s2.Point]s2.Point{}} }

func (s *snapper) add(p s2.Point) s2.Point {
	if r, ok := s.exact[p]; ok {
		return r
	}
	for _, r := range s.reps {
		if r.Distance(p) <= mergeDist {
			s.exact[p] = r
			return r
		}
	}
	s.exact[p] = p
	s.reps = append(s.reps, p)
	return p
}

// onEdge is whether p lies in the interior of edge ab.
func onEdge(p, a, b s2.Point) bool {
	if p == a || p == b || p.Distance(a) <= mergeDist || p.Distance(b) <= mergeDist {
		return false
	}
	return s2.DistanceFromSegment(p, a, b) <= mergeDist
}

// Split splits each edge of es at its crossings with every edge of cut (and
// at cut's vertices that lie on it), returning the pieces with their input
// ids, in input order. Vertices closer than about a micrometre are merged.
func Split(es, cut []Edge) []Edge {
	sn := newSnapper()
	for _, e := range es {
		sn.add(e.A)
		sn.add(e.B)
	}
	for _, e := range cut {
		sn.add(e.A)
		sn.add(e.B)
	}
	splits := make([][]s2.Point, len(es))
	for i, e := range es {
		for _, c := range cut {
			if e.A == e.B || c.A == c.B {
				continue
			}
			switch s2.CrossingSign(e.A, e.B, c.A, c.B) {
			case s2.Cross:
				splits[i] = append(splits[i], sn.add(s2.Intersection(e.A, e.B, c.A, c.B)))
			default:
				for _, p := range []s2.Point{c.A, c.B} {
					if onEdge(p, e.A, e.B) {
						splits[i] = append(splits[i], sn.add(p))
					}
				}
			}
		}
	}
	var out []Edge
	for i, e := range es {
		a, b := sn.add(e.A), sn.add(e.B)
		xs := splits[i]
		sort.SliceStable(xs, func(p, q int) bool { return a.Distance(xs[p]) < a.Distance(xs[q]) })
		prev := a
		for _, x := range append(xs, b) {
			if x == prev {
				continue
			}
			out = append(out, Edge{prev, x, e.ID})
			prev = x
		}
	}
	return out
}

// ErrDegenerate reports an overlay whose winding numbers could not be
// measured robustly.
var ErrDegenerate = errors.New("degenerate geometry")

type group struct {
	u, v s2.Point // canonical direction
	m    int      // net multiplicity along u->v
	id   int      // min input id
}

// Overlay returns the region of the sphere whose winding number, with
// respect to the given closed paths (each loop's last vertex joins its
// first), is at least k: the boundary of {w >= k}, as loops with the region
// on their left, each started and the loops ordered as S2Builder's
// GetDirectedLoops does. ref is a point whose winding number is refW. full
// is set when the region is the whole sphere.
func Overlay(loops [][]s2.Point, ref s2.Point, refW, k int) (out [][]s2.Point, full bool, err error) {
	var es []Edge
	id := 0
	for _, l := range loops {
		for i := range l {
			a, b := l[i], l[(i+1)%len(l)]
			if a != b {
				es = append(es, Edge{a, b, id})
			}
			id++
		}
	}
	if len(es) == 0 {
		return nil, refW >= k, nil
	}
	pieces := Split(es, es)
	type ukey struct{ a, b s2.Point }
	gi := map[ukey]int{}
	var gs []group
	for _, p := range pieces {
		k := ukey{p.A, p.B}
		dir := 1
		if pointLess(p.B, p.A) {
			k, dir = ukey{p.B, p.A}, -1
		}
		if i, ok := gi[k]; ok {
			gs[i].m += dir
			gs[i].id = minID(gs[i].id, p.ID)
			continue
		}
		gi[k] = len(gs)
		gs = append(gs, group{u: k.a, v: k.b, m: dir, id: p.ID})
	}
	var outEdges []Edge
	for i, g := range gs {
		if g.m == 0 {
			continue
		}
		wl, wr, err := sides(gs, i, ref, refW)
		if err != nil {
			return nil, false, err
		}
		switch {
		case wr < k && wl >= k:
			outEdges = append(outEdges, Edge{g.u, g.v, g.id})
		case wl < k && wr >= k:
			outEdges = append(outEdges, Edge{g.v, g.u, g.id})
		}
	}
	if len(outEdges) == 0 {
		// no boundary: the winding number is the same everywhere.
		return nil, refW >= k, nil
	}
	return assembleLoops(outEdges), false, nil
}

// sides are the winding numbers to the left and right of group i.
func sides(gs []group, i int, ref s2.Point, refW int) (int, int, error) {
	g := gs[i]
	for _, t := range []float64{0.5, 0.37, 0.61, 0.29, 0.73, 0.43} {
		m := s2.Interpolate(t, g.u, g.v)
		via := s2.Point{Vector: ref.Add(m.Vector)}
		if via.Norm2() < 0.5 {
			via = s2.Ortho(ref)
		} else {
			via = s2.Point{Vector: via.Normalize()}
		}
		// the side of g that a path arriving from via meets.
		side := s2.RobustSign(g.u, g.v, via)
		if side == s2.Indeterminate {
			continue
		}
		w, ok := refW, true
		for j, h := range gs {
			if j == i || h.m == 0 {
				continue
			}
			for _, seg := range [2][2]s2.Point{{ref, via}, {via, m}} {
				if seg[0] == seg[1] {
					continue
				}
				switch s2.CrossingSign(seg[0], seg[1], h.u, h.v) {
				case s2.Cross:
					if s2.RobustSign(h.u, h.v, seg[1]) == s2.CounterClockwise {
						w += h.m
					} else {
						w -= h.m
					}
				case s2.MaybeCross:
					ok = false
				}
			}
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		if side == s2.CounterClockwise {
			return w, w - g.m, nil
		}
		return w + g.m, w, nil
	}
	return 0, 0, ErrDegenerate
}

// assembleLoops joins directed edges into loops, turning as far left as
// possible where several edges leave a vertex, then canonicalizes each
// loop's start and the loops' order (Graph::CanonicalizeLoopOrder,
// CanonicalizeVectorOrder).
func assembleLoops(es []Edge) [][]s2.Point {
	var pts []s2.Point
	for _, e := range es {
		pts = append(pts, e.A, e.B)
	}
	g := newGraph(pts)
	sort.SliceStable(es, func(i, j int) bool {
		si, sj := g.id[es[i].A], g.id[es[j].A]
		if si != sj {
			return si < sj
		}
		return g.id[es[i].B] < g.id[es[j].B]
	})
	out := map[s2.Point][]int{}
	for i, e := range es {
		out[e.A] = append(out[e.A], i)
	}
	used := make([]bool, len(es))
	var loops [][]Edge
	for start := range es {
		if used[start] {
			continue
		}
		var path []Edge
		e := start
		for !used[e] {
			used[e] = true
			path = append(path, es[e])
			v, from := es[e].B, es[e].A
			next := -1
			for _, c := range out[v] {
				if used[c] {
					continue
				}
				// the first edge clockwise from the way back.
				if next < 0 || s2.OrderedCCW(es[next].B, es[c].B, from, v) {
					next = c
				}
			}
			if next < 0 {
				break
			}
			e = next
		}
		// peel off simple loops at repeated vertices.
		for len(path) > 0 {
			at := map[s2.Point]int{}
			cut := -1
			for i, pe := range path {
				if j, ok := at[pe.A]; ok {
					cut = j
					_ = i
					break
				}
				at[pe.A] = i
			}
			if cut < 0 {
				loops = append(loops, rotateEdges(path))
				break
			}
			// the loop from the repeat: find its end.
			end := cut
			for end < len(path) && !(end > cut && path[end].A == path[cut].A) {
				end++
			}
			loops = append(loops, rotateEdges(path[cut:end]))
			path = append(append([]Edge(nil), path[:cut]...), path[end:]...)
		}
	}
	sort.SliceStable(loops, func(a, b int) bool { return loops[a][0].ID < loops[b][0].ID })
	var res [][]s2.Point
	for _, l := range loops {
		vs := make([]s2.Point, len(l))
		for i, e := range l {
			vs[i] = e.A
		}
		res = append(res, vs)
	}
	return res
}
