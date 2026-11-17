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

// S2Builder, ported from src/s2/s2builder.{h,cc} (Eric Veach): snap
// rounding of input edges to sites chosen at least the snap function's
// minimum vertex separation apart, with Voronoi-site exclusion
// (SnapEdge), extra separation sites (AddExtraSites), optional splitting
// of crossing edges, edge-chain simplification (EdgeChainSimplifier, on
// S2PolylineSimplifier from src/s2/s2polyline_simplifier.cc), idempotency,
// labels and per-layer output graphs.
//
// Differences from upstream: the memory tracker is dropped; S2PointIndex /
// S2ClosestPointQuery are replaced by a cell grid (siteGrid) and
// S2ClosestEdgeQuery by golang/geo's EdgeQuery or a scan, each returning
// the same candidate set upstream's conservative queries do (every
// candidate is then tested with exact predicates as upstream does); the
// edges AddExtraSites re-snaps are visited in increasing id order (upstream
// iterates a hash set).

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/golang/geo/r3"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// BuilderOptions is S2Builder::Options. Setting SimplifyEdgeChains also
// makes the builder non-idempotent, as set_simplify_edge_chains does.
type BuilderOptions struct {
	SnapFunction       s2.Snapper
	SplitCrossingEdges bool
	// IntersectionTolerance is in radians (S1Angle).
	IntersectionTolerance float64
	SimplifyEdgeChains    bool
	Idempotent            bool
}

// DefaultBuilderOptions is S2Builder::Options(): IdentitySnapFunction(0),
// idempotent.
func DefaultBuilderOptions() BuilderOptions {
	return BuilderOptions{SnapFunction: s2.NewIdentitySnapper(0), Idempotent: true}
}

// intersectionTolerance is Options::intersection_tolerance().
func (o BuilderOptions) intersectionTolerance() s1.Angle {
	if !o.SplitCrossingEdges {
		return s1.Angle(o.IntersectionTolerance)
	}
	return max(s1.Angle(o.IntersectionTolerance), IntersectionError)
}

// EdgeSnapRadius is Options::edge_snap_radius().
func (o BuilderOptions) EdgeSnapRadius() s1.Angle {
	return o.SnapFunction.SnapRadius() + o.intersectionTolerance()
}

// MaxEdgeDeviation is Options::max_edge_deviation().
func (o BuilderOptions) MaxEdgeDeviation() s1.Angle {
	const maxEdgeDeviationRatio = 1.1
	return maxEdgeDeviationRatio * o.EdgeSnapRadius()
}

type inputEdge struct{ first, second InputVertexID }

// Builder is S2Builder.
type Builder struct {
	opts BuilderOptions

	siteSnapRadiusCA             s1.ChordAngle
	edgeSnapRadiusCA             s1.ChordAngle
	snappingRequested            bool
	maxEdgeDeviation             s1.Angle
	edgeSiteQueryRadiusCA        s1.ChordAngle
	minEdgeLengthToSplitCA       s1.ChordAngle
	checkAllSiteCrossings        bool
	minSiteSeparation            s1.Angle
	minSiteSeparationCA          s1.ChordAngle
	minEdgeSiteSeparationCA      s1.ChordAngle
	minEdgeSiteSeparationCALimit s1.ChordAngle
	maxAdjacentSiteSeparationCA  s1.ChordAngle
	edgeSnapRadiusSin2           float64

	inputVertices []s2.Point
	inputEdges    []inputEdge

	layers                       []Layer
	layerOptions                 []GraphOptions
	layerBegins                  []InputEdgeID
	layerIsFullPolygonPredicates []IsFullPolygonPredicate

	labelSetIDs      []LabelSetID
	labelSetLexicon  *IDSetLexicon
	labelSet         []Label
	labelSetID       LabelSetID
	labelSetModified bool

	snappingNeeded bool
	sites          []s2.Point
	edgeSites      [][]SiteID
	numForcedSites int

	err error
}

func roundUp(a s1.Angle) s1.ChordAngle {
	ca := s1.ChordAngleFromAngle(a)
	return ca.Expanded(ca.MaxAngleError())
}

func addPointToPointError(ca s1.ChordAngle) s1.ChordAngle {
	return ca.Expanded(ca.MaxPointError())
}

func addPointToEdgeError(ca s1.ChordAngle) s1.ChordAngle {
	return ca.Expanded(GetUpdateMinDistanceMaxError(ca))
}

// NewBuilder is S2Builder(options).
func NewBuilder(o BuilderOptions) *Builder {
	b := &Builder{labelSetLexicon: NewIDSetLexicon()}
	b.init(o)
	return b
}

// init is S2Builder::Init.
func (b *Builder) init(o BuilderOptions) {
	if o.SnapFunction == nil {
		o.SnapFunction = s2.NewIdentitySnapper(0)
	}
	if o.SimplifyEdgeChains {
		o.Idempotent = false
	}
	b.opts = o
	sf := o.SnapFunction
	snapRadius := sf.SnapRadius()
	b.siteSnapRadiusCA = s1.ChordAngleFromAngle(snapRadius)
	edgeSnapRadius := o.EdgeSnapRadius()
	b.edgeSnapRadiusCA = roundUp(edgeSnapRadius)
	b.snappingRequested = edgeSnapRadius > 0
	b.maxEdgeDeviation = o.MaxEdgeDeviation()
	b.edgeSiteQueryRadiusCA = s1.ChordAngleFromAngle(b.maxEdgeDeviation + sf.MinEdgeVertexSeparation())
	if !b.snappingRequested {
		b.minEdgeLengthToSplitCA = s1.InfChordAngle()
	} else {
		b.minEdgeLengthToSplitCA = s1.ChordAngleFromAngle(s1.Angle(
			2 * math.Acos(math.Sin(edgeSnapRadius.Radians())/math.Sin(b.maxEdgeDeviation.Radians()))))
	}
	b.checkAllSiteCrossings = o.MaxEdgeDeviation() > o.EdgeSnapRadius()+sf.MinEdgeVertexSeparation()
	b.minSiteSeparation = sf.MinVertexSeparation()
	b.minSiteSeparationCA = s1.ChordAngleFromAngle(b.minSiteSeparation)
	b.minEdgeSiteSeparationCA = s1.ChordAngleFromAngle(sf.MinEdgeVertexSeparation())
	b.minEdgeSiteSeparationCALimit = addPointToEdgeError(b.minEdgeSiteSeparationCA)
	b.maxAdjacentSiteSeparationCA = addPointToPointError(roundUp(2 * edgeSnapRadius))
	d := math.Sin(edgeSnapRadius.Radians())
	const dblEpsilon = 2 * dblErrS2
	b.edgeSnapRadiusSin2 = d * d
	b.edgeSnapRadiusSin2 += ((9.5*d+2.5+2*sqrt3)*d + 9*dblEpsilon) * dblEpsilon
	b.labelSetID = emptySetID
	b.labelSetModified = false
	b.snappingNeeded = false
}

// Options returns the builder's options.
func (b *Builder) Options() BuilderOptions { return b.opts }

func (b *Builder) isForced(v SiteID) bool { return int(v) < b.numForcedSites }

// ClearLabels is clear_labels.
func (b *Builder) ClearLabels() {
	b.labelSet = b.labelSet[:0]
	b.labelSetModified = true
}

// PushLabel is push_label.
func (b *Builder) PushLabel(l Label) {
	b.labelSet = append(b.labelSet, l)
	b.labelSetModified = true
}

// PopLabel is pop_label.
func (b *Builder) PopLabel() {
	b.labelSet = b.labelSet[:len(b.labelSet)-1]
	b.labelSetModified = true
}

// SetLabel is set_label.
func (b *Builder) SetLabel(l Label) {
	b.labelSet = append(b.labelSet[:0], l)
	b.labelSetModified = true
}

// StartLayer is StartLayer.
func (b *Builder) StartLayer(l Layer) {
	b.layerOptions = append(b.layerOptions, l.GraphOptions())
	b.layerBegins = append(b.layerBegins, InputEdgeID(len(b.inputEdges)))
	b.layerIsFullPolygonPredicates = append(b.layerIsFullPolygonPredicates, IsFullPolygon(false))
	b.layers = append(b.layers, l)
}

// addVertex is AddVertex.
func (b *Builder) addVertex(v s2.Point) InputVertexID {
	if len(b.inputVertices) == 0 || v != b.inputVertices[len(b.inputVertices)-1] {
		b.inputVertices = append(b.inputVertices, v)
	}
	return InputVertexID(len(b.inputVertices) - 1)
}

// AddIntersection is AddIntersection: adds an intersection point computed
// by the caller (with intersection_tolerance() > 0), which forces snapping.
func (b *Builder) AddIntersection(v s2.Point) {
	b.snappingNeeded = true
	b.addVertex(v)
}

// AddEdge is AddEdge.
func (b *Builder) AddEdge(v0, v1 s2.Point) {
	if len(b.layers) == 0 {
		panic("s2ops: call StartLayer before adding any edges")
	}
	if v0 == v1 && b.layerOptions[len(b.layerOptions)-1].DegenerateEdges == DegenerateEdgesDiscard {
		return
	}
	j0 := b.addVertex(v0)
	j1 := b.addVertex(v1)
	b.inputEdges = append(b.inputEdges, inputEdge{j0, j1})
	if b.labelSetModified {
		if len(b.labelSetIDs) == 0 {
			b.labelSetIDs = make([]LabelSetID, len(b.inputEdges)-1, len(b.inputEdges))
			for i := range b.labelSetIDs {
				b.labelSetIDs[i] = b.labelSetID
			}
		}
		b.labelSetID = b.labelSetLexicon.Add(b.labelSet...)
		b.labelSetIDs = append(b.labelSetIDs, b.labelSetID)
		b.labelSetModified = false
	} else if len(b.labelSetIDs) > 0 {
		b.labelSetIDs = append(b.labelSetIDs, b.labelSetID)
	}
}

// AddPoint adds a degenerate edge representing a point.
func (b *Builder) AddPoint(v s2.Point) { b.AddEdge(v, v) }

// AddPolyline is AddPolyline.
func (b *Builder) AddPolyline(vs []s2.Point) {
	for i := 1; i < len(vs); i++ {
		b.AddEdge(vs[i-1], vs[i])
	}
}

// AddLoop is AddLoop (the last vertex joins the first).
func (b *Builder) AddLoop(vs []s2.Point) {
	for i := range vs {
		b.AddEdge(vs[i], vs[(i+1)%len(vs)])
	}
}

// AddShape is AddShape.
func (b *Builder) AddShape(shape s2.Shape) {
	for e, n := 0, shape.NumEdges(); e < n; e++ {
		edge := shape.Edge(e)
		b.AddEdge(edge.V0, edge.V1)
	}
}

// AddIsFullPolygonPredicate is AddIsFullPolygonPredicate.
func (b *Builder) AddIsFullPolygonPredicate(p IsFullPolygonPredicate) {
	b.layerIsFullPolygonPredicates[len(b.layerIsFullPolygonPredicates)-1] = p
}

// ForceVertex is ForceVertex.
func (b *Builder) ForceVertex(v s2.Point) { b.sites = append(b.sites, v) }

// NumInputEdges is num_input_edges.
func (b *Builder) NumInputEdges() int { return len(b.inputEdges) }

// InputEdge is input_edge(e).
func (b *Builder) InputEdge(e InputEdgeID) (s2.Point, s2.Point) {
	ie := b.inputEdges[e]
	return b.inputVertices[ie.first], b.inputVertices[ie.second]
}

func (b *Builder) setError(err error) {
	if b.err == nil {
		b.err = err
	}
}

// Build is Build: snaps the input and builds every layer, returning the
// first error (S2Error) any step reported.
func (b *Builder) Build() error {
	b.err = nil
	b.layerBegins = append(b.layerBegins, InputEdgeID(len(b.inputEdges)))
	if b.snappingRequested && !b.opts.Idempotent {
		b.snappingNeeded = true
	}
	b.chooseSites()
	b.buildLayers()
	b.Reset()
	return b.err
}

// Reset is Reset.
func (b *Builder) Reset() {
	b.inputVertices = nil
	b.inputEdges = nil
	b.layers = nil
	b.layerOptions = nil
	b.layerBegins = nil
	b.layerIsFullPolygonPredicates = nil
	b.labelSetIDs = nil
	b.labelSetLexicon.Clear()
	b.labelSet = nil
	b.labelSetModified = false
	b.sites = nil
	b.edgeSites = nil
	b.snappingNeeded = false
}

// inputEdgeShape is VertexIdEdgeVectorShape: the input edges as a shape
// whose edge ids are input edge ids.
type inputEdgeShape struct {
	s2.Shape
	b *Builder
}

func (s *inputEdgeShape) NumEdges() int { return len(s.b.inputEdges) }
func (s *inputEdgeShape) Edge(e int) s2.Edge {
	ie := s.b.inputEdges[e]
	return s2.Edge{V0: s.b.inputVertices[ie.first], V1: s.b.inputVertices[ie.second]}
}
func (s *inputEdgeShape) Dimension() int                    { return 1 }
func (s *inputEdgeShape) ReferencePoint() s2.ReferencePoint { return s2.OriginReferencePoint(false) }
func (s *inputEdgeShape) NumChains() int                    { return len(s.b.inputEdges) }
func (s *inputEdgeShape) Chain(i int) s2.Chain              { return s2.Chain{Start: i, Length: 1} }
func (s *inputEdgeShape) ChainEdge(i, j int) s2.Edge        { return s.Edge(i) }
func (s *inputEdgeShape) ChainPosition(e int) s2.ChainPosition {
	return s2.ChainPosition{ChainID: e, Offset: 0}
}
func (s *inputEdgeShape) IsEmpty() bool { return len(s.b.inputEdges) == 0 }
func (s *inputEdgeShape) IsFull() bool  { return false }

func (b *Builder) inputEdgeIndex() *s2.ShapeIndex {
	idx := s2.NewShapeIndex()
	idx.Add(&inputEdgeShape{Shape: s2.LaxPolylineFromPoints(nil), b: b})
	return idx
}

// chooseSites is ChooseSites.
func (b *Builder) chooseSites() {
	if len(b.inputVertices) == 0 {
		return
	}
	if b.opts.SplitCrossingEdges {
		b.addEdgeCrossings()
	}
	if b.snappingRequested {
		grid := b.addForcedSites()
		b.chooseInitialSites(grid)
		b.collectSiteEdges()
	}
	if b.snappingNeeded {
		b.addExtraSites()
	} else {
		b.chooseAllVerticesAsSites()
	}
}

// chooseAllVerticesAsSites is ChooseAllVerticesAsSites.
func (b *Builder) chooseAllVerticesAsSites() {
	b.sites = b.sites[:0]
	sorted := b.sortInputVertices()
	vmap := make([]InputVertexID, len(b.inputVertices))
	for in := 0; in < len(sorted); {
		site := b.inputVertices[sorted[in].id]
		vmap[sorted[in].id] = InputVertexID(len(b.sites))
		for in++; in < len(sorted) && b.inputVertices[sorted[in].id] == site; in++ {
			vmap[sorted[in].id] = InputVertexID(len(b.sites))
		}
		b.sites = append(b.sites, site)
	}
	b.inputVertices = slices.Clone(b.sites)
	for i := range b.inputEdges {
		e := &b.inputEdges[i]
		e.first = vmap[e.first]
		e.second = vmap[e.second]
	}
}

type inputVertexKey struct {
	cell s2.CellID
	id   InputVertexID
}

// sortInputVertices is SortInputVertices: by leaf cell, then point, then id.
func (b *Builder) sortInputVertices() []inputVertexKey {
	keys := make([]inputVertexKey, len(b.inputVertices))
	for i, v := range b.inputVertices {
		keys[i] = inputVertexKey{cellIDFromPoint(v), InputVertexID(i)}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, c := keys[i], keys[j]
		if a.cell != c.cell {
			return a.cell < c.cell
		}
		pa, pc := b.inputVertices[a.id], b.inputVertices[c.id]
		if pa != pc {
			return pointLess(pa, pc)
		}
		return a.id < c.id
	})
	return keys
}

// addEdgeCrossings is AddEdgeCrossings: every interior crossing of two
// input edges becomes an input vertex.
func (b *Builder) addEdgeCrossings() {
	idx := b.inputEdgeIndex()
	shape := idx.Shape(0)
	q := s2.NewCrossingEdgeQuery(idx)
	var newVertices []s2.Point
	for a := range b.inputEdges {
		ea := shape.Edge(a)
		for _, c := range q.Crossings(ea.V0, ea.V1, shape, s2.CrossingTypeInterior) {
			if c <= a {
				continue
			}
			ec := shape.Edge(c)
			newVertices = append(newVertices, GetIntersection(ea.V0, ea.V1, ec.V0, ec.V1))
		}
	}
	if len(newVertices) == 0 {
		return
	}
	b.snappingNeeded = true
	b.inputVertices = append(b.inputVertices, newVertices...)
}

// siteGrid stands in for S2PointIndex<SiteId> + S2ClosestPointQuery: sites
// bucketed by the cell, at a level whose cells are at least the query
// radius wide, so every site within the radius of a point lies in the
// point's cell or a neighbour.
type siteGrid struct {
	level int
	all   bool // the radius is too large for any level: scan every site
	cells map[s2.CellID][]SiteID
	ids   []SiteID
}

func newSiteGrid(radius s1.ChordAngle) *siteGrid {
	r := radius.Angle().Radians()
	level := s2.MinWidthMetric.MaxLevel(r)
	g := &siteGrid{level: level, cells: map[s2.CellID][]SiteID{}}
	if s2.MinWidthMetric.Value(level) < r {
		g.all = true
	}
	return g
}

func (g *siteGrid) add(p s2.Point, id SiteID) {
	if g.all {
		g.ids = append(g.ids, id)
		return
	}
	c := cellIDFromPoint(p).Parent(g.level)
	g.cells[c] = append(g.cells[c], id)
}

func (g *siteGrid) near(p s2.Point, f func(SiteID)) {
	if g.all {
		for _, id := range g.ids {
			f(id)
		}
		return
	}
	c := cellIDFromPoint(p).Parent(g.level)
	for _, id := range g.cells[c] {
		f(id)
	}
	for _, n := range c.AllNeighbors(g.level) {
		for _, id := range g.cells[n] {
			f(id)
		}
	}
}

// conservativeLimit is S2ClosestPointQueryOptions::
// set_conservative_max_distance: results are those with distance < this.
func conservativeLimit(r s1.ChordAngle) s1.ChordAngle {
	return r.Expanded(GetUpdateMinDistanceMaxError(r)).Successor()
}

// addForcedSites is AddForcedSites.
func (b *Builder) addForcedSites() *siteGrid {
	slices.SortFunc(b.sites, func(x, y s2.Point) int {
		switch {
		case pointLess(x, y):
			return -1
		case pointLess(y, x):
			return 1
		}
		return 0
	})
	b.sites = slices.Compact(b.sites)
	grid := newSiteGrid(conservativeLimit(b.minSiteSeparationCA))
	for id, s := range b.sites {
		grid.add(s, SiteID(id))
	}
	b.numForcedSites = len(b.sites)
	return grid
}

// chooseInitialSites is ChooseInitialSites ("snap first").
func (b *Builder) chooseInitialSites(grid *siteGrid) {
	limit := conservativeLimit(b.minSiteSeparationCA)
	for _, key := range b.sortInputVertices() {
		vertex := b.inputVertices[key.id]
		site := b.snapSite(vertex)
		b.snappingNeeded = b.snappingNeeded || site != vertex
		addSite := true
		if b.siteSnapRadiusCA == 0 {
			addSite = len(b.sites) == 0 || site != b.sites[len(b.sites)-1]
		} else {
			grid.near(site, func(id SiteID) {
				p := b.sites[id]
				if s2.ChordAngleBetweenPoints(site, p) >= limit {
					return
				}
				if s2.CompareDistance(site, p, b.minSiteSeparationCA) <= 0 {
					addSite = false
					b.snappingNeeded = b.snappingNeeded || site != p
				}
			})
		}
		if addSite {
			grid.add(site, SiteID(len(b.sites)))
			b.sites = append(b.sites, site)
		}
	}
}

// snapSite is SnapSite.
func (b *Builder) snapSite(point s2.Point) s2.Point {
	if !b.snappingRequested {
		return point
	}
	site := b.opts.SnapFunction.SnapPoint(point)
	distMoved := s2.ChordAngleBetweenPoints(site, point)
	if distMoved > b.siteSnapRadiusCA {
		b.setError(errBuilder(fmt.Sprintf("Snap function moved vertex (%.15g, %.15g, %.15g) by %.15g, which is more than the specified snap radius of %.15g",
			point.X, point.Y, point.Z, distMoved.Angle().Radians(), b.siteSnapRadiusCA.Angle().Radians())))
	}
	return site
}

// collectSiteEdges is CollectSiteEdges.
func (b *Builder) collectSiteEdges() {
	idx := s2.NewShapeIndex()
	pv := s2.PointVector(slices.Clone(b.sites))
	idx.Add(&pv)
	opts := s2.NewClosestEdgeQueryOptions().DistanceLimit(conservativeLimit(b.edgeSiteQueryRadiusCA))
	q := s2.NewClosestEdgeQuery(idx, opts)
	b.edgeSites = make([][]SiteID, len(b.inputEdges))
	for e, edge := range b.inputEdges {
		v0, v1 := b.inputVertices[edge.first], b.inputVertices[edge.second]
		var sites []SiteID
		for _, r := range q.FindEdges(s2.NewMinDistanceToEdgeTarget(s2.Edge{V0: v0, V1: v1})) {
			id := SiteID(r.EdgeID())
			sites = append(sites, id)
			p := b.sites[id]
			if !b.snappingNeeded && r.Distance() < b.minEdgeSiteSeparationCALimit &&
				p != v0 && p != v1 &&
				CompareEdgeDistance(p, v0, v1, b.minEdgeSiteSeparationCA) < 0 {
				b.snappingNeeded = true
			}
		}
		b.sortSitesByDistance(v0, sites)
		b.edgeSites[e] = sites
	}
}

// sortSitesByDistance is SortSitesByDistance.
func (b *Builder) sortSitesByDistance(x s2.Point, sites []SiteID) {
	sort.Slice(sites, func(p, q int) bool {
		i, j := sites[p], sites[q]
		if i == j {
			return false
		}
		diff := s2.CompareDistances(x, b.sites[i], b.sites[j])
		if diff != 0 {
			return diff < 0
		}
		return i < j
	})
}

// insertSiteByDistance is InsertSiteByDistance.
func (b *Builder) insertSiteByDistance(newSite SiteID, x s2.Point, sites []SiteID) []SiteID {
	pos := sort.Search(len(sites), func(k int) bool {
		return !(s2.CompareDistances(x, b.sites[sites[k]], b.sites[newSite]) < 0)
	})
	return slices.Insert(sites, pos, newSite)
}

// addExtraSites is AddExtraSites.
func (b *Builder) addExtraSites() {
	edgesToResnap := map[InputEdgeID]bool{}
	var chain []SiteID
	check := func(e InputEdgeID) {
		chain = b.snapEdge(e, chain)
		delete(edgesToResnap, e)
		b.maybeAddExtraSites(e, chain, edgesToResnap)
	}
	for e := range b.inputEdges {
		check(InputEdgeID(e))
	}
	for len(edgesToResnap) > 0 {
		edgesToSnap := make([]InputEdgeID, 0, len(edgesToResnap))
		for e := range edgesToResnap {
			edgesToSnap = append(edgesToSnap, e)
		}
		slices.Sort(edgesToSnap)
		clear(edgesToResnap)
		for _, e := range edgesToSnap {
			check(e)
		}
	}
}

// maybeAddExtraSites is MaybeAddExtraSites.
func (b *Builder) maybeAddExtraSites(edgeID InputEdgeID, chain []SiteID, edgesToResnap map[InputEdgeID]bool) {
	if len(chain) == 0 {
		return
	}
	edge := b.inputEdges[edgeID]
	a0, a1 := b.inputVertices[edge.first], b.inputVertices[edge.second]
	nearby := b.edgeSites[edgeID]
	for i, j := 0, 0; j < len(nearby); j++ {
		id := nearby[j]
		if id == chain[i] {
			i++
			if i == len(chain) {
				break
			}
			v0, v1 := b.sites[chain[i-1]], b.sites[chain[i]]
			if s2.ChordAngleBetweenPoints(v0, v1) < b.minEdgeLengthToSplitCA {
				continue
			}
			if !IsEdgeBNearEdgeA(a0, a1, v0, v1, b.maxEdgeDeviation) {
				mid := s2.Point{Vector: normalizeNF(ProjectToEdge(v0, a0, a1).Add(ProjectToEdge(v1, a0, a1).Vector))}
				newSite := b.getSeparationSite(mid, v0, v1, edgeID)
				b.addExtraSite(newSite, edgesToResnap)
				return
			}
		} else {
			if i == 0 {
				continue
			}
			siteToAvoid := b.sites[id]
			v0, v1 := b.sites[chain[i-1]], b.sites[chain[i]]
			addSeparationSite := false
			if !b.isForced(id) && b.minEdgeSiteSeparationCA > 0 &&
				CompareEdgeDistance(siteToAvoid, v0, v1, b.minEdgeSiteSeparationCA) < 0 {
				addSeparationSite = true
			}
			if !addSeparationSite && (b.isForced(id) || b.checkAllSiteCrossings) &&
				sign(a0, a1, siteToAvoid) != sign(v0, v1, siteToAvoid) &&
				CompareEdgeDirections(a0, a1, a0, siteToAvoid) > 0 &&
				CompareEdgeDirections(a0, a1, siteToAvoid, a1) > 0 &&
				CompareEdgeDirections(a0, a1, v0, siteToAvoid) > 0 &&
				CompareEdgeDirections(a0, a1, siteToAvoid, v1) > 0 {
				addSeparationSite = true
			}
			if addSeparationSite {
				newSite := b.getSeparationSite(siteToAvoid, v0, v1, edgeID)
				b.addExtraSite(newSite, edgesToResnap)
				// addExtraSite may have grown this edge's list; re-read it.
				nearby = b.edgeSites[edgeID]
				for nearby[j+1] != chain[i] {
					j++
				}
			}
		}
	}
}

// addExtraSite is AddExtraSite.
func (b *Builder) addExtraSite(newSite s2.Point, edgesToResnap map[InputEdgeID]bool) {
	newSiteID := SiteID(len(b.sites))
	b.sites = append(b.sites, newSite)
	limit := conservativeLimit(b.edgeSiteQueryRadiusCA)
	for e, ie := range b.inputEdges {
		v0, v1 := b.inputVertices[ie.first], b.inputVertices[ie.second]
		d, _ := s2.UpdateMinDistance(newSite, v0, v1, s1.InfChordAngle())
		if d >= limit {
			continue
		}
		b.edgeSites[e] = b.insertSiteByDistance(newSiteID, v0, b.edgeSites[e])
		edgesToResnap[InputEdgeID(e)] = true
	}
}

// getSeparationSite is GetSeparationSite.
func (b *Builder) getSeparationSite(siteToAvoid, v0, v1 s2.Point, inputEdgeID InputEdgeID) s2.Point {
	edge := b.inputEdges[inputEdgeID]
	x, y := b.inputVertices[edge.first], b.inputVertices[edge.second]
	xyDir := y.Sub(x.Vector)
	n := RobustCrossProd(x, y)
	newSite := Project(siteToAvoid, x, y, n.Vector)
	gapMin := b.getCoverageEndpoint(v0, n)
	gapMax := b.getCoverageEndpoint(v1, s2.Point{Vector: n.Mul(-1)})
	if dotNF(newSite.Sub(gapMin.Vector), xyDir) < 0 {
		newSite = gapMin
	} else if dotNF(gapMax.Sub(newSite.Vector), xyDir) < 0 {
		newSite = gapMax
	}
	return b.snapSite(newSite)
}

// getCoverageEndpoint is GetCoverageEndpoint.
func (b *Builder) getCoverageEndpoint(p, n s2.Point) s2.Point {
	n2 := dotNF(n.Vector, n.Vector)
	nDp := dotNF(n.Vector, p.Vector)
	nXp := crossNF(n.Vector, p.Vector)
	nXpXn := r3.Vector{
		X: float64(n2*p.X) - float64(nDp*n.X),
		Y: float64(n2*p.Y) - float64(nDp*n.Y),
		Z: float64(n2*p.Z) - float64(nDp*n.Z),
	}
	s := math.Sqrt(1 - b.edgeSnapRadiusSin2)
	mr2 := float64(b.edgeSnapRadiusSin2*n2) - float64(nDp*nDp)
	m := math.Sqrt(math.Max(0, mr2))
	return s2.Point{Vector: normalizeNF(r3.Vector{
		X: float64(s*nXpXn.X) + float64(m*nXp.X),
		Y: float64(s*nXpXn.Y) + float64(m*nXp.Y),
		Z: float64(s*nXpXn.Z) + float64(m*nXp.Z),
	})}
}

// snapEdge is SnapEdge: the chain of sites input edge e snaps to.
func (b *Builder) snapEdge(e InputEdgeID, chain []SiteID) []SiteID {
	chain = chain[:0]
	edge := b.inputEdges[e]
	if !b.snappingNeeded {
		return append(chain, SiteID(edge.first), SiteID(edge.second))
	}
	x, y := b.inputVertices[edge.first], b.inputVertices[edge.second]
	for _, siteID := range b.edgeSites[e] {
		c := b.sites[siteID]
		if CompareEdgeDistance(c, x, y, b.edgeSnapRadiusCA) > 0 {
			continue
		}
		addSiteC := true
		for ; len(chain) > 0; chain = chain[:len(chain)-1] {
			bb := b.sites[chain[len(chain)-1]]
			if s2.ChordAngleBetweenPoints(bb, c) >= b.maxAdjacentSiteSeparationCA {
				break
			}
			result := GetVoronoiSiteExclusion(bb, c, x, y, b.edgeSnapRadiusCA)
			if result == ExcludedFirst {
				continue
			}
			if result == ExcludedSecond {
				addSiteC = false
				break
			}
			if len(chain) < 2 {
				break
			}
			a := b.sites[chain[len(chain)-2]]
			if s2.ChordAngleBetweenPoints(a, c) >= b.maxAdjacentSiteSeparationCA {
				break
			}
			xyb := sign(x, y, bb)
			if sign(a, bb, c) == xyb {
				break
			}
			if EdgeCircumcenterSign(x, y, a, bb, c) != xyb {
				break
			}
		}
		if addSiteC {
			chain = append(chain, siteID)
		}
	}
	return chain
}

// buildLayers is BuildLayers.
func (b *Builder) buildLayers() {
	layerEdges, layerInputEdgeIDs, lexicon := b.buildLayerEdges()
	var layerVertices [][]s2.Point
	const minLayersForVertexFiltering = 10
	if len(b.layers) >= minLayersForVertexFiltering {
		allow := true
		for _, o := range b.layerOptions {
			allow = allow && o.AllowVertexFiltering
		}
		if allow {
			layerVertices = make([][]s2.Point, len(b.layers))
			var tmp []VertexID
			for i := range b.layers {
				layerVertices[i] = FilterVertices(b.sites, &layerEdges[i], &tmp)
			}
		}
	}
	for i, l := range b.layers {
		vertices := b.sites
		if layerVertices != nil {
			vertices = layerVertices[i]
		}
		g := NewGraph(b.layerOptions[i], vertices, layerEdges[i], layerInputEdgeIDs[i], lexicon,
			b.labelSetIDs, b.labelSetLexicon, b.layerIsFullPolygonPredicates[i])
		if err := l.Build(g); err != nil {
			b.setError(err)
		}
	}
}

// buildLayerEdges is BuildLayerEdges.
func (b *Builder) buildLayerEdges() ([][]GraphEdge, [][]InputEdgeIDSetID, *IDSetLexicon) {
	lexicon := NewIDSetLexicon()
	var siteVertices [][]InputVertexID
	simplify := b.snappingNeeded && b.opts.SimplifyEdgeChains
	if simplify {
		siteVertices = make([][]InputVertexID, len(b.sites))
	}
	layerEdges := make([][]GraphEdge, len(b.layers))
	layerInputEdgeIDs := make([][]InputEdgeIDSetID, len(b.layers))
	for i := range b.layers {
		layerEdges[i], layerInputEdgeIDs[i] = b.addSnappedEdges(b.layerBegins[i], b.layerBegins[i+1],
			b.layerOptions[i], lexicon, siteVertices)
	}
	if simplify {
		b.simplifyEdgeChains(siteVertices, layerEdges, layerInputEdgeIDs, lexicon)
	}
	b.edgeSites = nil
	for i := range b.layers {
		if err := ProcessEdges(&b.layerOptions[i], &layerEdges[i], &layerInputEdgeIDs[i], lexicon); err != nil {
			b.setError(err)
		}
	}
	return layerEdges, layerInputEdgeIDs, lexicon
}

// addSnappedEdges is AddSnappedEdges.
func (b *Builder) addSnappedEdges(begin, end InputEdgeID, options GraphOptions, lexicon *IDSetLexicon, siteVertices [][]InputVertexID) ([]GraphEdge, []InputEdgeIDSetID) {
	discardDegenerate := options.DegenerateEdges == DegenerateEdgesDiscard
	var edges []GraphEdge
	var ids []InputEdgeIDSetID
	var chain []SiteID
	for e := begin; e < end; e++ {
		id := lexicon.Add(e)
		chain = b.snapEdge(e, chain)
		if len(chain) == 0 {
			continue
		}
		maybeAddInputVertex(b.inputEdges[e].first, chain[0], siteVertices)
		if len(chain) == 1 {
			if discardDegenerate {
				continue
			}
			edges, ids = addSnappedEdge(chain[0], chain[0], id, options.EdgeType, edges, ids)
		} else {
			maybeAddInputVertex(b.inputEdges[e].second, chain[len(chain)-1], siteVertices)
			for i := 1; i < len(chain); i++ {
				edges, ids = addSnappedEdge(chain[i-1], chain[i], id, options.EdgeType, edges, ids)
			}
		}
	}
	return edges, ids
}

func maybeAddInputVertex(v InputVertexID, id SiteID, siteVertices [][]InputVertexID) {
	if len(siteVertices) == 0 {
		return
	}
	vs := siteVertices[id]
	if len(vs) == 0 || vs[len(vs)-1] != v {
		siteVertices[id] = append(vs, v)
	}
}

func addSnappedEdge(src, dst SiteID, id InputEdgeIDSetID, edgeType EdgeType, edges []GraphEdge, ids []InputEdgeIDSetID) ([]GraphEdge, []InputEdgeIDSetID) {
	edges = append(edges, GraphEdge{src, dst})
	ids = append(ids, id)
	if edgeType == EdgeTypeUndirected {
		edges = append(edges, GraphEdge{dst, src})
		ids = append(ids, emptySetID)
	}
	return edges, ids
}

type layerEdgeID struct {
	layer int
	edge  EdgeID
}

// simplifyEdgeChains is SimplifyEdgeChains.
func (b *Builder) simplifyEdgeChains(siteVertices [][]InputVertexID, layerEdges [][]GraphEdge, layerInputEdgeIDs [][]InputEdgeIDSetID, lexicon *IDSetLexicon) {
	if len(b.layers) == 0 {
		return
	}
	mergedEdges, mergedIDs, mergedLayers := mergeLayerEdges(layerEdges, layerInputEdgeIDs)
	for i := range layerEdges {
		layerEdges[i] = layerEdges[i][:0]
		layerInputEdgeIDs[i] = layerInputEdgeIDs[i][:0]
	}
	opts := GraphOptions{EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep, true}
	g := NewGraph(opts, b.sites, mergedEdges, mergedIDs, lexicon, nil, nil, nil)
	s := newEdgeChainSimplifier(b, g, mergedLayers, siteVertices, layerEdges, layerInputEdgeIDs, lexicon)
	s.run()
}

// mergeLayerEdges is MergeLayerEdges: every layer's edges in one sorted
// vector (StableLessThan), with their layers.
func mergeLayerEdges(layerEdges [][]GraphEdge, layerInputEdgeIDs [][]InputEdgeIDSetID) ([]GraphEdge, []InputEdgeIDSetID, []int) {
	var order []layerEdgeID
	for i := range layerEdges {
		for e := range layerEdges[i] {
			order = append(order, layerEdgeID{i, EdgeID(e)})
		}
	}
	sort.Slice(order, func(p, q int) bool {
		ai, bi := order[p], order[q]
		a, c := layerEdges[ai.layer][ai.edge], layerEdges[bi.layer][bi.edge]
		if a != c {
			return edgeLess(a, c)
		}
		if ai.layer != bi.layer {
			return ai.layer < bi.layer
		}
		return ai.edge < bi.edge
	})
	edges := make([]GraphEdge, 0, len(order))
	ids := make([]InputEdgeIDSetID, 0, len(order))
	layers := make([]int, 0, len(order))
	for _, id := range order {
		edges = append(edges, layerEdges[id.layer][id.edge])
		ids = append(ids, layerInputEdgeIDs[id.layer][id.edge])
		layers = append(layers, id.layer)
	}
	return edges, ids, layers
}

// edgeChainSimplifier is S2Builder::EdgeChainSimplifier.
type edgeChainSimplifier struct {
	builder           *Builder
	g                 *Graph
	in                *VertexInMap
	out               *VertexOutMap
	edgeLayers        []int
	siteVertices      [][]InputVertexID
	layerEdges        [][]GraphEdge
	layerInputEdgeIDs [][]InputEdgeIDSetID
	lexicon           *IDSetLexicon
	layerBegins       []InputEdgeID
	isInterior        []bool
	used              []bool
	newEdges          []GraphEdge
	newInputEdgeIDs   []InputEdgeIDSetID
	newEdgeLayers     []int
}

func newEdgeChainSimplifier(b *Builder, g *Graph, edgeLayers []int, siteVertices [][]InputVertexID, layerEdges [][]GraphEdge, layerInputEdgeIDs [][]InputEdgeIDSetID, lexicon *IDSetLexicon) *edgeChainSimplifier {
	return &edgeChainSimplifier{
		builder: b, g: g, in: NewVertexInMap(g), out: NewVertexOutMap(g),
		edgeLayers: edgeLayers, siteVertices: siteVertices,
		layerEdges: layerEdges, layerInputEdgeIDs: layerInputEdgeIDs,
		lexicon: lexicon, layerBegins: b.layerBegins,
		isInterior: make([]bool, g.NumVertices()), used: make([]bool, g.NumEdges()),
	}
}

func (s *edgeChainSimplifier) outEdgeIDs(v0, v1 VertexID) []EdgeID {
	begin, end := s.out.EdgeIDsTo(v0, v1)
	ids := make([]EdgeID, 0, end-begin)
	for e := begin; e < end; e++ {
		ids = append(ids, e)
	}
	return ids
}

func (s *edgeChainSimplifier) run() {
	for v := 0; v < s.g.NumVertices(); v++ {
		s.isInterior[v] = s.isInteriorVertex(VertexID(v))
	}
	for e := 0; e < s.g.NumEdges(); e++ {
		if s.used[e] {
			continue
		}
		edge := s.g.Edge(EdgeID(e))
		if s.isInterior[edge.First] {
			continue
		}
		if !s.isInterior[edge.Second] {
			s.outputEdge(EdgeID(e))
		} else {
			s.simplifyChain(edge.First, edge.Second)
		}
	}
	for e := 0; e < s.g.NumEdges(); e++ {
		if s.used[e] {
			continue
		}
		edge := s.g.Edge(EdgeID(e))
		if edge.First == edge.Second {
			s.outputEdge(EdgeID(e))
		} else {
			s.simplifyChain(edge.First, edge.Second)
		}
	}
	for e := range s.newEdges {
		layer := s.newEdgeLayers[e]
		s.layerEdges[layer] = append(s.layerEdges[layer], s.newEdges[e])
		s.layerInputEdgeIDs[layer] = append(s.layerInputEdgeIDs[layer], s.newInputEdgeIDs[e])
	}
}

func (s *edgeChainSimplifier) outputEdge(e EdgeID) {
	s.newEdges = append(s.newEdges, s.g.Edge(e))
	s.newInputEdgeIDs = append(s.newInputEdgeIDs, s.g.InputEdgeIDSetID(e))
	s.newEdgeLayers = append(s.newEdgeLayers, s.edgeLayers[e])
	s.used[e] = true
}

func (s *edgeChainSimplifier) graphEdgeLayer(e EdgeID) int { return s.edgeLayers[e] }

func (s *edgeChainSimplifier) inputEdgeLayer(id InputEdgeID) int {
	// upper_bound(layer_begins_, id) - (begin + 1)
	return sort.Search(len(s.layerBegins), func(i int) bool { return s.layerBegins[i] > id }) - 1
}

// interiorVertexMatcher is EdgeChainSimplifier::InteriorVertexMatcher.
type interiorVertexMatcher struct {
	v0, v1, v2       VertexID
	n0, n1, n2       int
	excessOut        int
	tooManyEndpoints bool
}

func newInteriorVertexMatcher(v0 VertexID) *interiorVertexMatcher {
	return &interiorVertexMatcher{v0: v0, v1: -1, v2: -1}
}

func (m *interiorVertexMatcher) startLayer() { m.excessOut, m.n0, m.n1, m.n2 = 0, 0, 0, 0 }

func (m *interiorVertexMatcher) tally(v VertexID, outgoing bool) {
	if outgoing {
		m.excessOut++
	} else {
		m.excessOut--
	}
	if v == m.v0 {
		m.n0++
		return
	}
	if m.v1 < 0 {
		m.v1 = v
	}
	if m.v1 == v {
		m.n1++
		return
	}
	if m.v2 < 0 {
		m.v2 = v
	}
	if m.v2 == v {
		m.n2++
	} else {
		m.tooManyEndpoints = true
	}
}

func (m *interiorVertexMatcher) matches() bool {
	return !m.tooManyEndpoints && m.excessOut == 0 && m.n1 == m.n2 && (m.n0 == 0 || m.n1 > 0)
}

// isInteriorVertex is EdgeChainSimplifier::IsInterior.
func (s *edgeChainSimplifier) isInteriorVertex(v VertexID) bool {
	if s.out.Degree(v) == 0 {
		return false
	}
	if s.out.Degree(v) != s.in.Degree(v) {
		return false
	}
	if s.builder.isForced(v) {
		return false
	}
	var edges []EdgeID
	begin, end := s.out.EdgeIDs(v)
	for e := begin; e < end; e++ {
		edges = append(edges, e)
	}
	edges = append(edges, s.in.EdgeIDs(v)...)
	sort.SliceStable(edges, func(i, j int) bool { return s.graphEdgeLayer(edges[i]) < s.graphEdgeLayer(edges[j]) })
	matcher := newInteriorVertexMatcher(v)
	for i := 0; i < len(edges); {
		layer := s.graphEdgeLayer(edges[i])
		matcher.startLayer()
		for ; i < len(edges) && s.graphEdgeLayer(edges[i]) == layer; i++ {
			edge := s.g.Edge(edges[i])
			if edge.First == v {
				matcher.tally(edge.Second, true)
			}
			if edge.Second == v {
				matcher.tally(edge.First, false)
			}
		}
		if !matcher.matches() {
			return false
		}
	}
	return true
}

// simplifyChain is EdgeChainSimplifier::SimplifyChain.
func (s *edgeChainSimplifier) simplifyChain(v0, v1 VertexID) {
	var chain []VertexID
	usedVertices := map[VertexID]bool{}
	var simplifier polylineSimplifier
	vstart := v0
	done := false
	for {
		chain = append(chain, v0)
		usedVertices[v0] = true
		simplifier.init(s.g.Vertex(v0))
		simplify := s.avoidSites(v0, v0, v1, usedVertices, &simplifier)
		for {
			chain = append(chain, v1)
			usedVertices[v1] = true
			done = !s.isInterior[v1] || v1 == vstart
			if done {
				break
			}
			vprev := v0
			v0 = v1
			v1 = s.followChain(vprev, v0)
			if !(simplify && s.targetInputVertices(v0, &simplifier) &&
				s.avoidSites(chain[0], v0, v1, usedVertices, &simplifier) &&
				simplifier.extend(s.g.Vertex(v1))) {
				break
			}
		}
		if len(chain) == 2 {
			s.outputAllEdges(chain[0], chain[1])
		} else {
			s.mergeChain(chain)
		}
		chain = chain[:0]
		clear(usedVertices)
		if done {
			break
		}
	}
}

// followChain is EdgeChainSimplifier::FollowChain.
func (s *edgeChainSimplifier) followChain(v0, v1 VertexID) VertexID {
	begin, end := s.out.EdgeIDs(v1)
	for e := begin; e < end; e++ {
		v := s.g.Edge(e).Second
		if v != v0 && v != v1 {
			return v
		}
	}
	panic("s2ops: could not find next edge in edge chain")
}

func (s *edgeChainSimplifier) outputAllEdges(v0, v1 VertexID) {
	for _, e := range s.outEdgeIDs(v0, v1) {
		s.outputEdge(e)
	}
	for _, e := range s.outEdgeIDs(v1, v0) {
		s.outputEdge(e)
	}
}

func (s *edgeChainSimplifier) targetInputVertices(v VertexID, simplifier *polylineSimplifier) bool {
	for _, i := range s.siteVertices[v] {
		if !simplifier.targetDisc(s.builder.inputVertices[i], s.builder.edgeSnapRadiusCA) {
			return false
		}
	}
	return true
}

func (s *edgeChainSimplifier) avoidSites(v0, v1, v2 VertexID, usedVertices map[VertexID]bool, simplifier *polylineSimplifier) bool {
	p0, p1, p2 := s.g.Vertex(v0), s.g.Vertex(v1), s.g.Vertex(v2)
	r1 := s2.ChordAngleBetweenPoints(p0, p1)
	r2 := s2.ChordAngleBetweenPoints(p0, p2)
	if r2 < r1 {
		return false
	}
	if r2 >= s.builder.minEdgeLengthToSplitCA {
		return false
	}
	best := InputEdgeID(-1)
	edgeSites := s.builder.edgeSites
	for _, pair := range [][2]VertexID{{v1, v2}, {v2, v1}} {
		for _, e := range s.outEdgeIDs(pair[0], pair[1]) {
			for _, id := range s.g.InputEdgeIDs(e) {
				if best < 0 || len(edgeSites[id]) < len(edgeSites[best]) {
					best = id
				}
			}
		}
	}
	for _, v := range edgeSites[best] {
		p := s.g.Vertex(v)
		r := s2.ChordAngleBetweenPoints(p0, p)
		if r >= r2 {
			continue
		}
		if usedVertices[v] {
			continue
		}
		usedVertices[v] = true
		var discOnLeft bool
		if v1 == v0 {
			discOnLeft = sign(p1, p2, p) > 0
		} else {
			discOnLeft = s2.OrderedCCW(p0, p2, p, p1)
		}
		if !simplifier.avoidDisc(p, s.builder.minEdgeSiteSeparationCA, discOnLeft) {
			return false
		}
	}
	return true
}

// mergeChain is EdgeChainSimplifier::MergeChain.
func (s *edgeChainSimplifier) mergeChain(vertices []VertexID) {
	var mergedInputIDs [][]InputEdgeID
	var degenerateIDs []InputEdgeID
	for i := 1; i < len(vertices); i++ {
		v0, v1 := vertices[i-1], vertices[i]
		outEdges := s.outEdgeIDs(v0, v1)
		inEdges := s.outEdgeIDs(v1, v0)
		if i == 1 {
			mergedInputIDs = make([][]InputEdgeID, len(outEdges)+len(inEdges))
		} else {
			for _, e := range s.outEdgeIDs(v0, v0) {
				degenerateIDs = append(degenerateIDs, s.g.InputEdgeIDs(e)...)
				s.used[e] = true
			}
		}
		j := 0
		for _, e := range outEdges {
			mergedInputIDs[j] = append(mergedInputIDs[j], s.g.InputEdgeIDs(e)...)
			s.used[e] = true
			j++
		}
		for _, e := range inEdges {
			mergedInputIDs[j] = append(mergedInputIDs[j], s.g.InputEdgeIDs(e)...)
			s.used[e] = true
			j++
		}
	}
	if len(degenerateIDs) > 0 {
		slices.Sort(degenerateIDs)
		s.assignDegenerateEdges(degenerateIDs, mergedInputIDs)
	}
	v0, v1, vb := vertices[0], vertices[1], vertices[len(vertices)-1]
	for _, e := range s.outEdgeIDs(v0, v1) {
		s.newEdges = append(s.newEdges, GraphEdge{v0, vb})
		s.newEdgeLayers = append(s.newEdgeLayers, s.graphEdgeLayer(e))
	}
	for _, e := range s.outEdgeIDs(v1, v0) {
		s.newEdges = append(s.newEdges, GraphEdge{vb, v0})
		s.newEdgeLayers = append(s.newEdgeLayers, s.graphEdgeLayer(e))
	}
	for _, ids := range mergedInputIDs {
		s.newInputEdgeIDs = append(s.newInputEdgeIDs, s.lexicon.Add(ids...))
	}
}

// assignDegenerateEdges is EdgeChainSimplifier::AssignDegenerateEdges.
func (s *edgeChainSimplifier) assignDegenerateEdges(degenerateIDs []InputEdgeID, mergedIDs [][]InputEdgeID) {
	for _, ids := range mergedIDs {
		slices.Sort(ids)
	}
	var order []int
	for i := range mergedIDs {
		if len(mergedIDs[i]) > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return mergedIDs[order[i]][0] < mergedIDs[order[j]][0] })
	for _, degenerateID := range degenerateIDs {
		layer := s.inputEdgeLayer(degenerateID)
		it := sort.Search(len(order), func(k int) bool { return degenerateID < mergedIDs[order[k]][0] })
		if it != 0 {
			if mergedIDs[order[it-1]][0] >= s.layerBegins[layer] {
				it--
			}
		}
		mergedIDs[order[it]] = append(mergedIDs[order[it]], degenerateID)
	}
}

// polylineSimplifier is S2PolylineSimplifier (src/s2/s2polyline_simplifier.cc).
type polylineSimplifier struct {
	src           s2.Point
	xDir, yDir    s2.Point
	window        s1.Interval
	rangesToAvoid []rangeToAvoid
}

type rangeToAvoid struct {
	interval s1.Interval
	onLeft   bool
}

// intervalFromPoint is S1Interval::FromPoint.
func intervalFromPoint(p float64) s1.Interval {
	if p == -math.Pi {
		p = math.Pi
	}
	return s1.Interval{Lo: p, Hi: p}
}

func (ps *polylineSimplifier) init(src s2.Point) {
	ps.src = src
	ps.window = s1.FullInterval()
	ps.rangesToAvoid = ps.rangesToAvoid[:0]
	tmp := src.Abs()
	var i int
	if tmp.X < tmp.Y {
		if tmp.X < tmp.Z {
			i = 0
		} else {
			i = 2
		}
	} else {
		if tmp.Y < tmp.Z {
			i = 1
		} else {
			i = 2
		}
	}
	j := i + 1
	if i == 2 {
		j = 0
	}
	k := i - 1
	if i == 0 {
		k = 2
	}
	at := func(v s2.Point, n int) float64 {
		switch n {
		case 0:
			return v.X
		case 1:
			return v.Y
		}
		return v.Z
	}
	var y, x [3]float64
	y[i] = 0
	y[j] = at(src, k)
	y[k] = -at(src, j)
	x[i] = float64(at(src, j)*at(src, j)) + float64(at(src, k)*at(src, k))
	x[j] = -at(src, j) * at(src, i)
	x[k] = -at(src, k) * at(src, i)
	ps.yDir.X, ps.yDir.Y, ps.yDir.Z = y[0], y[1], y[2]
	ps.xDir.X, ps.xDir.Y, ps.xDir.Z = x[0], x[1], x[2]
}

func (ps *polylineSimplifier) extend(dst s2.Point) bool {
	if s2.ChordAngleBetweenPoints(ps.src, dst) > s1.RightChordAngle {
		return false
	}
	dir := ps.getDirection(dst)
	if !ps.window.Contains(dir) {
		return false
	}
	for _, r := range ps.rangesToAvoid {
		if r.interval.Contains(dir) {
			return false
		}
	}
	return true
}

func (ps *polylineSimplifier) targetDisc(p s2.Point, r s1.ChordAngle) bool {
	semiwidth := ps.getSemiwidth(p, r, -1)
	if semiwidth >= math.Pi {
		return true
	}
	if semiwidth < 0 {
		ps.window = s1.EmptyInterval()
		return false
	}
	center := ps.getDirection(p)
	target := intervalFromPoint(center).Expanded(semiwidth)
	ps.window = ps.window.Intersection(target)
	for _, r := range ps.rangesToAvoid {
		ps.avoidRange(r.interval, r.onLeft)
	}
	ps.rangesToAvoid = ps.rangesToAvoid[:0]
	return !ps.window.IsEmpty()
}

func (ps *polylineSimplifier) avoidDisc(p s2.Point, r s1.ChordAngle, discOnLeft bool) bool {
	semiwidth := ps.getSemiwidth(p, r, 1)
	if semiwidth >= math.Pi {
		ps.window = s1.EmptyInterval()
		return false
	}
	center := ps.getDirection(p)
	dleft, dright := semiwidth, math.Pi/2
	if discOnLeft {
		dleft, dright = math.Pi/2, semiwidth
	}
	avoid := s1.IntervalFromEndpoints(math.Remainder(center-dright, 2*math.Pi), math.Remainder(center+dleft, 2*math.Pi))
	if ps.window.IsFull() {
		ps.rangesToAvoid = append(ps.rangesToAvoid, rangeToAvoid{avoid, discOnLeft})
		return true
	}
	ps.avoidRange(avoid, discOnLeft)
	return !ps.window.IsEmpty()
}

func (ps *polylineSimplifier) avoidRange(avoid s1.Interval, discOnLeft bool) {
	if ps.window.ContainsInterval(avoid) {
		if discOnLeft {
			ps.window = s1.IntervalFromEndpoints(ps.window.Lo, avoid.Lo)
		} else {
			ps.window = s1.IntervalFromEndpoints(avoid.Hi, ps.window.Hi)
		}
	} else {
		ps.window = ps.window.Intersection(avoid.Complement())
	}
}

func (ps *polylineSimplifier) getDirection(p s2.Point) float64 {
	return math.Atan2(dotNF(p.Vector, ps.yDir.Vector), dotNF(p.Vector, ps.xDir.Vector))
}

func (ps *polylineSimplifier) getSemiwidth(p s2.Point, r s1.ChordAngle, roundDirection int) float64 {
	r2 := float64(r)
	a2 := float64(s2.ChordAngleBetweenPoints(ps.src, p))
	a2 -= 64 * dblErrS2 * dblErrS2 * float64(roundDirection)
	if a2 <= r2 {
		return math.Pi
	}
	sin2R := r2 * (1 - 0.25*r2)
	sin2A := a2 * (1 - 0.25*a2)
	semiwidth := math.Asin(math.Sqrt(sin2R / sin2A))
	e := (2*10+4)*dblErrS2 + 17*dblErrS2*semiwidth
	return semiwidth + float64(roundDirection)*e
}

// cellIDFromPoint is S2CellId(point), the leaf cell containing p.
func cellIDFromPoint(p s2.Point) s2.CellID { return s2.CellFromPoint(p).ID() }
