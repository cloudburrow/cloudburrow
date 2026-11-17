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

// Package s2ops ports the S2 operations github.com/golang/geo does not
// (#1728, #1735) from Google's S2 Geometry Library
// (https://github.com/google/s2geometry, commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd, Apache License 2.0), on
// golang/geo's types and exact predicates: S2BooleanOperation
// (boolean_operation.go), S2Builder with its snapping and edge-chain
// simplification (builder.go), S2Builder::Graph (builder_graph.go), the
// S2PolygonLayer, S2PolylineVectorLayer and S2PointVectorLayer layers and
// the ClosedSetNormalizer (builder_layers.go), S2WindingOperation
// (winding_operation.go), S2BufferOperation's offset curves (buffer.go), and
// the predicates and crossing helpers golang/geo lacks (predicates.go,
// crossings.go, shapeutil.go). PROVENANCE.md records each file's upstream
// source and what was changed.
package s2ops
