/*
 * Copyright 2015 Google Inc. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package assemble provides functions to build the various components (nodes,
// edges, and decorations) of an xrefs serving table.
package assemble

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"

	"kythe.io/kythe/go/services/graphstore"
	"kythe.io/kythe/go/services/xrefs"
	"kythe.io/kythe/go/util/encoding/text"
	"kythe.io/kythe/go/util/kytheuri"
	"kythe.io/kythe/go/util/pager"
	"kythe.io/kythe/go/util/schema"

	"golang.org/x/net/context"

	cpb "kythe.io/kythe/proto/common_proto"
	ipb "kythe.io/kythe/proto/internal_proto"
	srvpb "kythe.io/kythe/proto/serving_proto"
	spb "kythe.io/kythe/proto/storage_proto"
	xpb "kythe.io/kythe/proto/xref_proto"
)

// Source is a collection of facts and edges with a common source.
type Source struct {
	Ticket string

	Facts map[string][]byte
	Edges map[string][]EdgeTarget
}

// EdgeTarget is a target of an edge with an optional ordinal.
type EdgeTarget struct {
	Ticket  string
	Ordinal int32
}

// Node returns the Source as a srvpb.Node.
func (s *Source) Node() *srvpb.Node {
	facts := make([]*cpb.Fact, 0, len(s.Facts))
	for name, value := range s.Facts {
		facts = append(facts, &cpb.Fact{Name: name, Value: value})
	}
	sort.Sort(xrefs.ByName(facts))
	return &srvpb.Node{
		Ticket: s.Ticket,
		Fact:   facts,
	}
}

// SourceFromEntries returns a new Source from the given a set of entries with
// the same source VName.
func SourceFromEntries(entries []*spb.Entry) *Source {
	if len(entries) == 0 {
		return nil
	}

	src := &Source{
		Ticket: kytheuri.ToString(entries[0].Source),
		Facts:  make(map[string][]byte),
		Edges:  make(map[string][]EdgeTarget),
	}

	// edge kind -> target ticket -> ordinal set
	edges := make(map[string]map[string]map[int32]struct{})

	for _, e := range entries {
		if graphstore.IsEdge(e) {
			kind, ordinal, _ := schema.ParseOrdinal(e.EdgeKind)
			tgts, ok := edges[kind]
			if !ok {
				tgts = make(map[string]map[int32]struct{})
				edges[kind] = tgts
			}

			ticket := kytheuri.ToString(e.Target)
			ordSet, ok := tgts[ticket]
			if !ok {
				ordSet = make(map[int32]struct{})
				tgts[ticket] = ordSet
			}
			ordSet[int32(ordinal)] = struct{}{}
		} else {
			src.Facts[e.FactName] = e.FactValue
		}
	}
	for kind, targets := range edges {
		for target, ordinals := range targets {
			for ordinal := range ordinals {
				src.Edges[kind] = append(src.Edges[kind], EdgeTarget{
					Ticket:  target,
					Ordinal: ordinal,
				})
			}
		}
		sort.Sort(byOrdinal(src.Edges[kind]))
	}

	return src
}

// FactsToMap returns a map from fact name to value.
func FactsToMap(facts []*cpb.Fact) map[string][]byte {
	m := make(map[string][]byte, len(facts))
	for _, f := range facts {
		m[f.Name] = f.Value
	}
	return m
}

// GetFact returns the value of the first fact in facts with the given name; otherwise returns nil.
func GetFact(facts []*cpb.Fact, name string) []byte {
	for _, f := range facts {
		if f.Name == name {
			return f.Value
		}
	}
	return nil
}

// PartialReverseEdges returns the set of partial reverse edges from the given source.  Each
// reversed Edge has its Target fully populated and its Source will have no facts.  To ensure every
// node has at least 1 Edge, the first Edge will be a self-edge without a Kind or Target.  To reduce
// the size of edge sets, each Target will have any text facts filtered (see FilterTextFacts).
func PartialReverseEdges(src *Source) []*srvpb.Edge {
	node := src.Node()

	edges := []*srvpb.Edge{{
		Source: node, // self-edge to ensure every node has at least 1 edge
	}}

	targetNode := FilterTextFacts(node)

	for kind, targets := range src.Edges {
		rev := schema.MirrorEdge(kind)
		for _, target := range targets {
			edges = append(edges, &srvpb.Edge{
				Source:  &srvpb.Node{Ticket: target.Ticket},
				Kind:    rev,
				Ordinal: target.Ordinal,
				Target:  targetNode,
			})
		}
	}

	return edges
}

// FilterTextFacts returns a new Node without any text facts.
func FilterTextFacts(n *srvpb.Node) *srvpb.Node {
	res := &srvpb.Node{
		Ticket: n.Ticket,
		Fact:   make([]*cpb.Fact, 0, len(n.Fact)),
	}
	for _, f := range n.Fact {
		switch f.Name {
		case schema.TextFact, schema.TextEncodingFact:
			// Skip large text facts for targets
		default:
			res.Fact = append(res.Fact, f)
		}
	}
	return res
}

// DecorationFragmentBuilder builds pieces of FileDecorations given an ordered (see AddEdge) stream
// of completed Edges.  Each fragment constructed (either by AddEdge or Flush) will be emitted using
// the Output function in the builder.  There are two types of fragments: file fragments (which have
// their SourceText, FileTicket, and Encoding set) and decoration fragments (which have only
// Decoration set).
type DecorationFragmentBuilder struct {
	Output func(ctx context.Context, file string, fragment *srvpb.FileDecorations) error

	anchor  *srvpb.RawAnchor
	decor   []*srvpb.FileDecorations_Decoration
	parents []string
}

// AddEdge adds the given edge to the current fragment (or emits some fragments and starts a new
// fragment with e).  AddEdge must be called in GraphStore sorted order of the Edges with the
// beginning to every set of edges with the same Source having a signaling Edge with only its Source
// set (no Kind or Target).  Otherwise, every Edge must have a completed Source, Kind, and Target.
// Flush must be called after every call to AddEdge in order to output any remaining fragments.
func (b *DecorationFragmentBuilder) AddEdge(ctx context.Context, e *srvpb.Edge) error {
	if e.Target == nil {
		// Beginning of a set of edges with a new Source
		if err := b.Flush(ctx); err != nil {
			return err
		}

		srcFacts := FactsToMap(e.Source.Fact)

		switch string(srcFacts[schema.NodeKindFact]) {
		case schema.FileKind:
			if err := b.Output(ctx, e.Source.Ticket, &srvpb.FileDecorations{
				File: &srvpb.File{
					Ticket:   e.Source.Ticket,
					Text:     srcFacts[schema.TextFact],
					Encoding: string(srcFacts[schema.TextEncodingFact]),
				},
			}); err != nil {
				return err
			}
		case schema.AnchorKind:
			// Implicit anchors don't belong in file decorations.
			if string(srcFacts[schema.SubkindFact]) == schema.ImplicitSubkind {
				return nil
			}
			anchorStart, err := strconv.Atoi(string(srcFacts[schema.AnchorStartFact]))
			if err != nil {
				log.Printf("Error parsing anchor start offset %q: %v",
					string(srcFacts[schema.AnchorStartFact]), err)
				return nil
			}
			anchorEnd, err := strconv.Atoi(string(srcFacts[schema.AnchorEndFact]))
			if err != nil {
				log.Printf("Error parsing anchor end offset %q: %v",
					string(srcFacts[schema.AnchorEndFact]), err)
				return nil
			}

			// Ignore errors; offsets will just be zero
			snippetStart, _ := strconv.Atoi(string(srcFacts[schema.SnippetStartFact]))
			snippetEnd, _ := strconv.Atoi(string(srcFacts[schema.SnippetEndFact]))

			b.anchor = &srvpb.RawAnchor{
				Ticket:       e.Source.Ticket,
				StartOffset:  int32(anchorStart),
				EndOffset:    int32(anchorEnd),
				SnippetStart: int32(snippetStart),
				SnippetEnd:   int32(snippetEnd),
			}
		}
		return nil
	} else if b.anchor == nil {
		// We don't care about edges for non-anchors
		return nil
	}

	if e.Kind == schema.ChildOfEdge {
		if string(GetFact(e.Target.Fact, schema.NodeKindFact)) == schema.FileKind {
			b.parents = append(b.parents, e.Target.Ticket)
		}
	} else {
		b.decor = append(b.decor, &srvpb.FileDecorations_Decoration{
			Anchor: b.anchor,
			Kind:   e.Kind,
			Target: e.Target,
		})

		if len(b.parents) > 0 {
			fd := &srvpb.FileDecorations{Decoration: b.decor}
			for _, parent := range b.parents {
				if err := b.Output(ctx, parent, fd); err != nil {
					return err
				}
			}
			b.decor = nil
		}
	}

	return nil
}

// Flush outputs any remaining fragments that are being built.  It is safe, but usually unnecessary,
// to call Flush in between sets of Edges with the same Source.  This also means that
// DecorationFragmentBuilder can be used to construct decoration fragments in parallel by
// partitioning edges along the same boundaries.
func (b *DecorationFragmentBuilder) Flush(ctx context.Context) error {
	defer func() {
		b.anchor = nil
		b.decor = nil
		b.parents = nil
	}()

	if len(b.decor) > 0 && len(b.parents) > 0 {
		fd := &srvpb.FileDecorations{Decoration: b.decor}
		for _, parent := range b.parents {
			if err := b.Output(ctx, parent, fd); err != nil {
				return err
			}
		}
	}
	return nil
}

// ByOffset sorts file decorations by their byte offsets.
type ByOffset []*srvpb.FileDecorations_Decoration

func (s ByOffset) Len() int      { return len(s) }
func (s ByOffset) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s ByOffset) Less(i, j int) bool {
	if s[i].Anchor.StartOffset < s[j].Anchor.StartOffset {
		return true
	} else if s[i].Anchor.StartOffset > s[j].Anchor.StartOffset {
		return false
	} else if s[i].Anchor.EndOffset == s[j].Anchor.EndOffset {
		if s[i].Kind == s[j].Kind {
			return s[i].Target.Ticket < s[j].Target.Ticket
		}
		return s[i].Kind < s[j].Kind
	}
	return s[i].Anchor.EndOffset < s[j].Anchor.EndOffset
}

// EdgeSetBuilder constructs a set of PagedEdgeSets and EdgePages from a
// sequence of Nodes and EdgeSet_Groups.  For each set of groups with the same
// source, a call to StartEdgeSet must precede.  All EdgeSet_Groups for the same
// source are then assumed to be given sequentially to AddGroup, secondarily
// ordered by the group's edge kind.  If given in this order, Output will be
// given exactly 1 PagedEdgeSet per source with as few EdgeSet_Group per edge
// kind as to satisfy MaxEdgePageSize (MaxEdgePageSize == 0 indicates that there
// will be exactly 1 edge group per edge kind).  If not given in this order, no
// guarantees can be made.  Flush must be called after the final call to
// AddGroup.
type EdgeSetBuilder struct {
	// MaxEdgePageSize is maximum number of edges that are allowed in the
	// PagedEdgeSet and any EdgePage.  If MaxEdgePageSize <= 0, no paging is
	// attempted.
	MaxEdgePageSize int

	// Output is used to emit each PagedEdgeSet constructed.
	Output func(context.Context, *srvpb.PagedEdgeSet) error
	// OutputPage is used to emit each EdgePage constructed.
	OutputPage func(context.Context, *srvpb.EdgePage) error

	pager *pager.SetPager
}

func (b *EdgeSetBuilder) constructPager() *pager.SetPager {
	// Head:  *srvpb.Node
	// Set:   *srvpb.PagedEdgeSet
	// Group: *srvpb.EdgeGroup
	return &pager.SetPager{
		MaxPageSize: b.MaxEdgePageSize,

		NewSet: func(hd pager.Head) pager.Set {
			return &srvpb.PagedEdgeSet{
				Source: hd.(*srvpb.Node),
			}
		},
		Combine: func(l, r pager.Group) pager.Group {
			lg, rg := l.(*srvpb.EdgeGroup), r.(*srvpb.EdgeGroup)
			if lg.Kind != rg.Kind {
				return nil
			}
			lg.Edge = append(lg.Edge, rg.Edge...)
			return lg
		},
		Split: func(sz int, g pager.Group) (l, r pager.Group) {
			eg := g.(*srvpb.EdgeGroup)
			neg := &srvpb.EdgeGroup{
				Kind: eg.Kind,
				Edge: eg.Edge[:sz],
			}
			eg.Edge = eg.Edge[sz:]
			return neg, eg
		},
		Size: func(g pager.Group) int { return len(g.(*srvpb.EdgeGroup).Edge) },

		OutputSet: func(ctx context.Context, total int, s pager.Set, grps []pager.Group) error {
			pes := s.(*srvpb.PagedEdgeSet)

			// pes.Group = []*srvpb.EdgeGroup(grps)
			pes.Group = make([]*srvpb.EdgeGroup, len(grps))
			for i, g := range grps {
				pes.Group[i] = g.(*srvpb.EdgeGroup)
			}

			sort.Sort(byEdgeKind(pes.Group))
			sort.Sort(byPageKind(pes.PageIndex))
			pes.TotalEdges = int32(total)

			return b.Output(ctx, pes)
		},
		OutputPage: func(ctx context.Context, s pager.Set, g pager.Group) error {
			pes := s.(*srvpb.PagedEdgeSet)
			eviction := g.(*srvpb.EdgeGroup)

			src := pes.Source.Ticket
			key := newPageKey(src, len(pes.PageIndex))

			// Output the EdgePage and add it to the page indices
			if err := b.OutputPage(ctx, &srvpb.EdgePage{
				PageKey:      key,
				SourceTicket: src,
				EdgesGroup:   eviction,
			}); err != nil {
				return fmt.Errorf("error emitting EdgePage: %v", err)
			}
			pes.PageIndex = append(pes.PageIndex, &srvpb.PageIndex{
				PageKey:   key,
				EdgeKind:  eviction.Kind,
				EdgeCount: int32(len(eviction.Edge)),
			})
			return nil
		},
	}
}

// StartEdgeSet begins a new EdgeSet for the given source node, possibly
// emitting a PagedEdgeSet for the previous EdgeSet.  Each following call to
// AddGroup adds the group to this new EdgeSet until another call to
// StartEdgeSet is made.
func (b *EdgeSetBuilder) StartEdgeSet(ctx context.Context, src *srvpb.Node) error {
	if b.pager == nil {
		b.pager = b.constructPager()
	}
	return b.pager.StartSet(ctx, src)
}

// AddGroup adds a EdgeSet_Group to current EdgeSet being built, possibly
// emitting a new PagedEdgeSet and/or EdgePage.  StartEdgeSet must be called
// before any calls to this method.  See EdgeSetBuilder's documentation for the
// assumed order of the groups and this method's relation to StartEdgeSet.
func (b *EdgeSetBuilder) AddGroup(ctx context.Context, eg *srvpb.EdgeGroup) error {
	return b.pager.AddGroup(ctx, eg)
}

// Flush signals the end of the current PagedEdgeSet being built, flushing it,
// and its EdgeSet_Groups to the output function.  This should be called after
// the final call to AddGroup.  Manually calling Flush at any other time is
// unnecessary.
func (b *EdgeSetBuilder) Flush(ctx context.Context) error { return b.pager.Flush(ctx) }

// CrossReferencesBuilder is a type wrapper around a pager.SetPager that emits
// *srvpb.PagedCrossReferences and *srvpb.PagedCrossReferences_Pages.  Each
// PagedCrossReferences_Group added the builder should be in sorted order so
// that groups of the same kind are added sequentially.  Before each set of
// like-kinded groups, StartSet should be called with the source ticket of the
// proceeding groups.  See also EdgeSetBuilder.
type CrossReferencesBuilder struct {
	MaxPageSize int

	Output     func(context.Context, *srvpb.PagedCrossReferences) error
	OutputPage func(context.Context, *srvpb.PagedCrossReferences_Page) error

	pager *pager.SetPager
}

func (b *CrossReferencesBuilder) constructPager() *pager.SetPager {
	// Head:  *srvpb.Node
	// Set:   *srvpb.PagedCrossReferences
	// Group: *srvpb.PagedCrossReferences_Group
	// Page:  *srvpb.PagedCrossReferences_Page
	return &pager.SetPager{
		MaxPageSize: b.MaxPageSize,

		NewSet: func(hd pager.Head) pager.Set {
			n := hd.(*srvpb.Node)
			var incomplete bool
			for _, f := range n.Fact {
				if f.Name == schema.CompleteFact && string(f.Value) != "definition" {
					incomplete = true
				}
			}
			return &srvpb.PagedCrossReferences{
				SourceTicket: n.Ticket,
				Incomplete:   incomplete,
			}
		},
		Combine: func(l, r pager.Group) pager.Group {
			lg, rg := l.(*srvpb.PagedCrossReferences_Group), r.(*srvpb.PagedCrossReferences_Group)
			if lg.Kind != rg.Kind {
				return nil
			}
			lg.Anchor = append(lg.Anchor, rg.Anchor...)
			return lg
		},
		Split: func(sz int, g pager.Group) (l, r pager.Group) {
			og := g.(*srvpb.PagedCrossReferences_Group)
			ng := &srvpb.PagedCrossReferences_Group{
				Kind:   og.Kind,
				Anchor: og.Anchor[:sz],
			}
			og.Anchor = og.Anchor[sz:]
			return ng, og
		},
		Size: func(g pager.Group) int { return len(g.(*srvpb.PagedCrossReferences_Group).Anchor) },

		OutputSet: func(ctx context.Context, total int, s pager.Set, grps []pager.Group) error {
			xs := s.(*srvpb.PagedCrossReferences)

			// xs.Group = grps.([]*srvpb.PagedCrossReferences_Group)
			xs.Group = make([]*srvpb.PagedCrossReferences_Group, len(grps))
			for i, g := range grps {
				xs.Group[i] = g.(*srvpb.PagedCrossReferences_Group)
			}

			sort.Sort(byRefKind(xs.Group))
			sort.Sort(byRefPageKind(xs.PageIndex))
			xs.TotalReferences = int32(total)

			return b.Output(ctx, xs)
		},
		OutputPage: func(ctx context.Context, s pager.Set, g pager.Group) error {
			xs, xg := s.(*srvpb.PagedCrossReferences), g.(*srvpb.PagedCrossReferences_Group)

			key := newPageKey(xs.SourceTicket, len(xs.PageIndex))

			pg := &srvpb.PagedCrossReferences_Page{
				PageKey:      key,
				SourceTicket: xs.SourceTicket,
				Group:        xg,
			}
			xs.PageIndex = append(xs.PageIndex, &srvpb.PagedCrossReferences_PageIndex{
				PageKey: key,
				Kind:    xg.Kind,
				Count:   int32(len(xg.Anchor)),
			})
			return b.OutputPage(ctx, pg)
		},
	}
}

// StartSet begins a new *srvpb.PagedCrossReferences.  As a side-effect, a
// previously-built srvpb.PagedCrossReferences may be emitted.
func (b *CrossReferencesBuilder) StartSet(ctx context.Context, src *srvpb.Node) error {
	if b.pager == nil {
		b.pager = b.constructPager()
	}
	return b.pager.StartSet(ctx, src)
}

// AddGroup add the given group of cross-references to the currently being built
// *srvpb.PagedCrossReferences.  The group should share the same source ticket
// as given to the mostly recent invocation to StartSet.
func (b *CrossReferencesBuilder) AddGroup(ctx context.Context, g *srvpb.PagedCrossReferences_Group) error {
	return b.pager.AddGroup(ctx, g)
}

// Flush emits any *srvpb.PagedCrossReferences and
// *srvpb.PagedCrossReferences_Page currently being built.
func (b *CrossReferencesBuilder) Flush(ctx context.Context) error { return b.pager.Flush(ctx) }

func newPageKey(src string, n int) string { return fmt.Sprintf("%s.%.10d", src, n) }

// CrossReference returns a (Referent, TargetAnchor) *ipb.CrossReference
// equivalent to the given decoration.  The decoration's anchor is expanded
// given its parent file and associated Normalizer.
func CrossReference(file *srvpb.File, norm *xrefs.Normalizer, d *srvpb.FileDecorations_Decoration) (*ipb.CrossReference, error) {
	if file == nil || norm == nil {
		return nil, errors.New("missing decoration's parent file")
	}

	ea, err := ExpandAnchor(d.Anchor, file, norm, schema.MirrorEdge(d.Kind))
	if err != nil {
		return nil, fmt.Errorf("error expanding anchor {%+v}: %v", d.Anchor, err)
	}
	// Throw away most of the referent's facts.  They are not needed.
	var facts []*cpb.Fact
	for _, fact := range d.Target.Fact {
		if fact.Name == schema.CompleteFact {
			facts = append(facts, fact)
		}
	}
	return &ipb.CrossReference{
		Referent: &srvpb.Node{
			Ticket: d.Target.Ticket,
			Fact:   facts,
		},
		TargetAnchor: ea,
	}, nil
}

// ExpandAnchor returns the ExpandedAnchor equivalent of the given RawAnchor
// where file (and its associated Normalizer) must be the anchor's parent file.
func ExpandAnchor(anchor *srvpb.RawAnchor, file *srvpb.File, norm *xrefs.Normalizer, kind string) (*srvpb.ExpandedAnchor, error) {
	if err := checkSpan(len(file.Text), anchor.StartOffset, anchor.EndOffset); err != nil {
		return nil, fmt.Errorf("invalid text offsets: %v", err)
	}

	sp := norm.ByteOffset(anchor.StartOffset)
	ep := norm.ByteOffset(anchor.EndOffset)
	txt, err := getText(sp, ep, file)
	if err != nil {
		return nil, fmt.Errorf("error getting anchor text: %v", err)
	}

	var snippet string
	var ssp, sep *xpb.Location_Point
	if anchor.SnippetStart != 0 || anchor.SnippetEnd != 0 {
		if err := checkSpan(len(file.Text), anchor.SnippetStart, anchor.SnippetEnd); err != nil {
			return nil, fmt.Errorf("invalid snippet offsets: %v", err)
		}

		ssp = norm.ByteOffset(anchor.SnippetStart)
		sep = norm.ByteOffset(anchor.SnippetEnd)
		snippet, err = getText(ssp, sep, file)
		if err != nil {
			return nil, fmt.Errorf("error getting text for snippet: %v", err)
		}
	} else {
		// fallback to a line-based snippet if the indexer did not provide its own snippet offsets
		ssp = &xpb.Location_Point{
			ByteOffset: sp.ByteOffset - sp.ColumnOffset,
			LineNumber: sp.LineNumber,
		}
		nextLine := norm.Point(&xpb.Location_Point{LineNumber: sp.LineNumber + 1})
		if nextLine.ByteOffset <= ssp.ByteOffset { // double-check ssp != EOF
			return nil, errors.New("anchor past EOF")
		}
		sep = &xpb.Location_Point{
			ByteOffset:   nextLine.ByteOffset - 1,
			LineNumber:   sp.LineNumber,
			ColumnOffset: sp.ColumnOffset + (nextLine.ByteOffset - sp.ByteOffset - 1),
		}
		snippet, err = getText(ssp, sep, file)
		if err != nil {
			return nil, fmt.Errorf("error getting text for line snippet: %v", err)
		}
	}

	return &srvpb.ExpandedAnchor{
		Ticket: anchor.Ticket,
		Kind:   kind,
		Parent: file.Ticket,

		Text: txt,
		Span: &cpb.Span{
			Start: p2p(sp),
			End:   p2p(ep),
		},

		Snippet: snippet,
		SnippetSpan: &cpb.Span{
			Start: p2p(ssp),
			End:   p2p(sep),
		},
	}, nil
}

func checkSpan(textLen int, start, end int32) error {
	if int(end) > textLen {
		return fmt.Errorf("span past EOF %d: [%d, %d)", textLen, start, end)
	} else if start < 0 {
		return fmt.Errorf("negative span: [%d, %d)", start, end)
	} else if start > end {
		return fmt.Errorf("crossed span: [%d, %d)", start, end)
	}
	return nil
}

func getText(sp, ep *xpb.Location_Point, file *srvpb.File) (string, error) {
	txt, err := text.ToUTF8(file.Encoding, file.Text[sp.ByteOffset:ep.ByteOffset])
	if err != nil {
		return "", fmt.Errorf("unable to decode file text: %v", err)
	}
	return txt, nil
}

func p2p(p *xpb.Location_Point) *cpb.Point {
	return &cpb.Point{
		ByteOffset:   p.ByteOffset,
		LineNumber:   p.LineNumber,
		ColumnOffset: p.ColumnOffset,
	}
}

var edgeOrdering = []string{
	schema.DefinesEdge,
	schema.DocumentsEdge,
	schema.RefEdge,
	schema.NamedEdge,
	schema.TypedEdge,
}

func edgeKindLess(kind1, kind2 string) bool {
	// General ordering:
	//   anchor edge kinds before non-anchor edge kinds
	//   forward edges before reverse edges
	//   edgeOrdering[i] (and variants) before edgeOrdering[i+1:]
	//   edge variants after root edge kind (ordered lexicographically)
	//   otherwise, order lexicographically

	if kind1 == kind2 {
		return false
	} else if a1, a2 := schema.IsAnchorEdge(kind1), schema.IsAnchorEdge(kind2); a1 != a2 {
		return a1
	} else if d1, d2 := schema.EdgeDirection(kind1), schema.EdgeDirection(kind2); d1 != d2 {
		return d1 == schema.Forward
	}
	kind1, kind2 = schema.Canonicalize(kind1), schema.Canonicalize(kind2)
	for _, kind := range edgeOrdering {
		if kind1 == kind {
			return true
		} else if kind2 == kind {
			return false
		} else if v1, v2 := schema.IsEdgeVariant(kind1, kind), schema.IsEdgeVariant(kind2, kind); v1 != v2 {
			return v1
		} else if v1 {
			return kind1 < kind2
		}
	}
	return kind1 < kind2
}

// byPageKind implements the sort.Interface
type byPageKind []*srvpb.PageIndex

// Implement the sort.Interface using edgeKindLess
func (s byPageKind) Len() int           { return len(s) }
func (s byPageKind) Less(i, j int) bool { return edgeKindLess(s[i].EdgeKind, s[j].EdgeKind) }
func (s byPageKind) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// byEdgeKind implements the sort.Interface
type byEdgeKind []*srvpb.EdgeGroup

// Implement the sort.Interface using edgeKindLess
func (s byEdgeKind) Len() int           { return len(s) }
func (s byEdgeKind) Less(i, j int) bool { return edgeKindLess(s[i].Kind, s[j].Kind) }
func (s byEdgeKind) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// byRefPageKind implements the sort.Interface
type byRefPageKind []*srvpb.PagedCrossReferences_PageIndex

// Implement the sort.Interface using edgeKindLess
func (s byRefPageKind) Len() int           { return len(s) }
func (s byRefPageKind) Less(i, j int) bool { return edgeKindLess(s[i].Kind, s[j].Kind) }
func (s byRefPageKind) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// byRefKind implements the sort.Interface
type byRefKind []*srvpb.PagedCrossReferences_Group

// Implement the sort.Interface using edgeKindLess
func (s byRefKind) Len() int           { return len(s) }
func (s byRefKind) Less(i, j int) bool { return edgeKindLess(s[i].Kind, s[j].Kind) }
func (s byRefKind) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// byOrdinal sorts edges by their ordinals
type byOrdinal []EdgeTarget

func (s byOrdinal) Len() int      { return len(s) }
func (s byOrdinal) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s byOrdinal) Less(i, j int) bool {
	if s[i].Ordinal == s[j].Ordinal {
		return s[i].Ticket < s[j].Ticket
	}
	return s[i].Ordinal < s[j].Ordinal
}
