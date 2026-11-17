// Copyright 2020 Google Inc. All Rights Reserved.
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

// Package s2ops builds the S2 operations github.com/golang/geo does not port
// (#1728): polygon overlay by winding number (the role of S2BooleanOperation
// and S2WindingOperation), buffering (S2BufferOperation) and snap-rounded
// edge-chain simplification (S2Builder's simplify_edge_chains), on the
// sphere, with golang/geo's exact predicates.
//
// Parts are ported from Google's S2 Geometry Library
// (https://github.com/google/s2geometry, commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd, Apache License 2.0):
// buffer.go follows src/s2/s2buffer_operation.cc closely (offset curves and
// the reference point's winding number), and order.go ports
// S2Builder::Graph's CanonicalizeLoopOrder, CanonicalizeVectorOrder and
// PolylineBuilder::BuildPaths from src/s2/s2builder_graph.cc, so that output
// loops and polylines start and run as S2Builder's do. The overlay
// (overlay.go) and the simplifier (simplify.go) are not ports: the overlay
// splits every edge at its crossings, merges coincident edges, and keeps the
// edges whose two sides' winding numbers straddle the threshold; the
// simplifier chooses sites as S2Builder::ChooseInitialSites does for an
// IdentitySnapFunction, snaps vertices to them, and removes chain vertices
// greedily. PROVENANCE.md records what was taken and changed.
package s2ops
