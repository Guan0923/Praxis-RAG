package retrieval

import (
	"testing"
)

// lexicalTestDocs is a small Chinese/English mixed corpus used by the lexical
// index and SAG2 tests. Passage a1 is the only initial BM25 hit for the
// question "Alice 出生在哪里？"; b1 is a bridge passage that shares
// "蒙特贝洛" with a1 but no query term, so it can only enter through expansion.
func lexicalTestDocs() []LexicalDoc {
	return []LexicalDoc{
		{
			ChunkID: "a1", DocumentID: "doc-a", Title: "Alice 出生地", Source: "a.md",
			Text:     "Alice 出生在蒙特贝洛。蒙特贝洛是纽约州罗克兰县的一个村庄。",
			Metadata: map[string]any{"topic": "geo"},
		},
		{
			ChunkID: "b1", DocumentID: "doc-b", Title: "Montebello 村庄", Source: "b.md",
			Text: "蒙特贝洛是美国纽约州罗克兰县下辖的一个村庄。",
		},
		{
			ChunkID: "c1", DocumentID: "doc-c", Title: "Bob 面包店", Source: "c.md",
			Text: "Bob 是蒙特贝洛一家面包店的员工。",
		},
		{
			ChunkID: "d1", DocumentID: "doc-d", Title: "Carol 地方史", Source: "d.md",
			Text: "Carol 研究纽约州的地方史。",
		},
		{
			ChunkID: "e1", DocumentID: "doc-e", Title: "Dan 音乐", Source: "e.md",
			Text: "Dan 是一位钢琴家。",
		},
		{
			ChunkID: "f1", DocumentID: "doc-f", Title: "Eve 运动", Source: "f.md",
			Text: "Eve 喜欢足球和篮球。",
		},
		{
			ChunkID: "g1", DocumentID: "doc-g", Title: "Frank 书籍", Source: "g.md",
			Text: "Frank 写了一本关于家乡地名的书。",
		},
		{
			ChunkID: "h1", DocumentID: "doc-h", Title: "Grace 旅行", Source: "h.md",
			Text: "Grace 计划去纽约州旅行。",
		},
		{
			ChunkID: "i1", DocumentID: "doc-i", Title: "Heidi 摄影", Source: "i.md",
			Text: "Heidi 热爱村庄摄影。",
		},
		{
			ChunkID: "j1", DocumentID: "doc-j", Title: "Ivan 教学", Source: "j.md",
			Text: "Ivan 于罗克兰县教书。",
		},
		{
			ChunkID: "k1", DocumentID: "doc-k", Title: "Judy 地图", Source: "k.md",
			Text: "Judy 绘制了纽约州的地图。",
		},
		{
			ChunkID: "l1", DocumentID: "doc-l", Title: "Ken 档案", Source: "l.md",
			Text: "Ken 整理了村庄的档案。",
		},
		{
			ChunkID: "m1", DocumentID: "doc-m", Title: "Lena 县志", Source: "m.md",
			Text: "Lena 编写了罗克兰县的县志。",
		},
	}
}

func newLexicalTestIndex(t *testing.T) *LexicalIndex {
	t.Helper()
	return NewLexicalIndex(lexicalTestDocs())
}

func TestLexicalIndexStatistics(t *testing.T) {
	index := newLexicalTestIndex(t)
	if got, want := index.Len(), 13; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	if got, want := index.N(), 13; got != want {
		t.Fatalf("N() = %d, want %d", got, want)
	}
	// "蒙特" appears in the bodies of a1, b1 and c1.
	if got, want := index.DF("蒙特"), 3; got != want {
		t.Fatalf("DF(蒙特) = %d, want %d", got, want)
	}
	// "钢琴" only appears in e1.
	if got, want := index.DF("钢琴"), 1; got != want {
		t.Fatalf("DF(钢琴) = %d, want %d", got, want)
	}
	if got := index.DF("完全不存在的词"); got != 0 {
		t.Fatalf("DF(unknown) = %d, want 0", got)
	}
	if index.AvgBodyLength() <= 0 {
		t.Fatalf("AvgBodyLength() = %v, want > 0", index.AvgBodyLength())
	}
	if index.AvgTitleLength() <= 0 {
		t.Fatalf("AvgTitleLength() = %v, want > 0", index.AvgTitleLength())
	}
	if _, ok := index.Doc(0); !ok {
		t.Fatal("Doc(0) reported missing")
	}
	if _, ok := index.Doc(99); ok {
		t.Fatal("Doc(99) reported present")
	}
	if got := index.Key(0); got != "a1" {
		t.Fatalf("Key(0) = %q, want a1", got)
	}

	empty := NewLexicalIndex(nil)
	if empty.Len() != 0 || empty.N() != 0 {
		t.Fatalf("empty index Len() = %d, want 0", empty.Len())
	}
	if got := empty.DF("任意"); got != 0 {
		t.Fatalf("empty DF() = %d, want 0", got)
	}
	if hits := empty.Search([]string{"任意"}, nil, 10); hits != nil {
		t.Fatalf("empty Search() = %v, want nil", hits)
	}
	if hits := empty.ExpansionTerms(nil, nil, 10); hits != nil {
		t.Fatalf("empty ExpansionTerms() = %v, want nil", hits)
	}
}

func TestLexicalIndexRejectsDuplicateKeys(t *testing.T) {
	index := NewLexicalIndex([]LexicalDoc{
		{ChunkID: "same", Text: "第一个文档"},
		{ChunkID: "same", Text: "重复的文档"},
		{Text: "没有标识的文档"},
		{Text: "第二个没有标识的文档"},
	})
	if got, want := index.Len(), 3; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	if got, want := index.Key(1), "#1"; got != want {
		t.Fatalf("Key(1) = %q, want %q", got, want)
	}
}

func TestLexicalIndexSearchOrdersAndExcludes(t *testing.T) {
	index := newLexicalTestIndex(t)
	queryTerms := lexicalQueryTerms("Alice 出生在哪里？")
	hits := index.Search(queryTerms, nil, 10)
	if len(hits) != 1 || hits[0].ChunkID != "a1" {
		t.Fatalf("Search() = %+v, want only a1", hits)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("a1 score = %v, want > 0", hits[0].Score)
	}
	if len(hits[0].Matched) == 0 {
		t.Fatal("a1 Matched terms are empty")
	}

	blocked := index.Search(queryTerms, map[string]struct{}{"a1": {}}, 10)
	if len(blocked) != 0 {
		t.Fatalf("excluded Search() = %+v, want empty", blocked)
	}

	// "蒙特贝洛" matches a1, b1 and c1; ordering must be score descending.
	bridgeHits := index.Search(lexicalQueryTerms("蒙特贝洛"), nil, 10)
	if len(bridgeHits) != 3 {
		t.Fatalf("Search(蒙特贝洛) returned %d hits, want 3", len(bridgeHits))
	}
	for position := 1; position < len(bridgeHits); position++ {
		if bridgeHits[position-1].Score < bridgeHits[position].Score {
			t.Fatalf("hits are not ordered by descending score: %+v", bridgeHits)
		}
	}
	limited := index.Search(lexicalQueryTerms("蒙特贝洛"), nil, 2)
	if len(limited) != 2 {
		t.Fatalf("limited Search() returned %d hits, want 2", len(limited))
	}
	if got := index.Search(nil, nil, 10); got != nil {
		t.Fatalf("Search(nil) = %v, want nil", got)
	}
	if got := index.Search([]string{"蒙特"}, nil, 0); got != nil {
		t.Fatalf("Search(limit=0) = %v, want nil", got)
	}
}

func TestLexicalIndexSearchTerm(t *testing.T) {
	index := newLexicalTestIndex(t)
	rankingTerms := append(lexicalQueryTerms("Alice 出生在哪里？"), "蒙特")
	hits := index.SearchTerm("蒙特", rankingTerms, nil, 10)
	found := make(map[string]bool, len(hits))
	for _, hit := range hits {
		found[hit.ChunkID] = true
	}
	for _, want := range []string{"a1", "b1", "c1"} {
		if !found[want] {
			t.Fatalf("SearchTerm(蒙特) is missing %s: %+v", want, found)
		}
	}
	for chunkID := range found {
		if chunkID != "a1" && chunkID != "b1" && chunkID != "c1" {
			t.Fatalf("SearchTerm(蒙特) returned a passage without the term: %s", chunkID)
		}
	}
	// A passage that shares no query term still enters through its own term.
	village := index.SearchTerm("村庄", lexicalQueryTerms("Alice 出生在哪里？"), nil, 10)
	villageIDs := make(map[string]bool, len(village))
	for _, hit := range village {
		villageIDs[hit.ChunkID] = true
	}
	if !villageIDs["i1"] {
		t.Fatalf("SearchTerm(村庄) is missing i1: %+v", villageIDs)
	}
	if got := index.SearchTerm("不存在的术语", rankingTerms, nil, 10); got != nil {
		t.Fatalf("SearchTerm(unknown) = %v, want nil", got)
	}
}

func TestLexicalIndexExpansionTerms(t *testing.T) {
	index := newLexicalTestIndex(t)
	queryTerms := lexicalQueryTerms("Alice 出生在哪里？")
	querySet := make(map[string]struct{}, len(queryTerms))
	for _, term := range queryTerms {
		querySet[term] = struct{}{}
	}
	terms := index.ExpansionTerms([]ExpansionSeed{{Index: 0, Score: 0.9}}, querySet, 20)
	if len(terms) == 0 {
		t.Fatal("ExpansionTerms() returned no terms for the seed passage")
	}
	bridge := false
	for position, term := range terms {
		if _, isQueryTerm := querySet[term.Term]; isQueryTerm {
			t.Fatalf("expansion term %q is also a query term", term.Term)
		}
		if term.DF == 0 {
			t.Fatalf("expansion term %q has zero document frequency", term.Term)
		}
		if term.Score <= 0 {
			t.Fatalf("expansion term %q has non-positive score %v", term.Term, term.Score)
		}
		if position > 0 && terms[position-1].Score < term.Score {
			t.Fatalf("expansion terms are not ordered by descending score")
		}
		if term.Term == "蒙特" || term.Term == "村庄" {
			bridge = true
		}
	}
	if !bridge {
		t.Fatalf("expected a bridge term such as 蒙特 or 村庄, got %+v", terms)
	}
	// Seed scores scale the term weight: the better seed must not lose.
	weighted := index.ExpansionTerms([]ExpansionSeed{{Index: 0, Score: 0.2}, {Index: 1, Score: 1.0}}, querySet, 20)
	if len(weighted) == 0 {
		t.Fatal("ExpansionTerms() returned no terms for two seeds")
	}
	if weighted[0].SeedWeight < 0.5 || weighted[0].SeedWeight > 1 {
		t.Fatalf("seed weight %v outside [0.5, 1]", weighted[0].SeedWeight)
	}
	if got := index.ExpansionTerms(nil, querySet, 20); got != nil {
		t.Fatalf("ExpansionTerms(no seeds) = %v, want nil", got)
	}
}

func TestLexicalIndexRejectedKeys(t *testing.T) {
	index := newLexicalTestIndex(t)
	if got := index.RejectedKeys(nil); got != nil {
		t.Fatalf("RejectedKeys(nil) = %v, want nil", got)
	}
	rejected := index.RejectedKeys(map[string]any{"document_id": "doc-b"})
	if got, want := len(rejected), 12; got != want {
		t.Fatalf("RejectedKeys() size = %d, want %d", got, want)
	}
	if _, ok := rejected["a1"]; !ok {
		t.Fatal("a1 should be rejected by the doc-b filter")
	}
	if _, ok := rejected["b1"]; ok {
		t.Fatal("b1 should match the doc-b filter")
	}
	byTitle := index.RejectedKeys(map[string]any{"title": "Alice 出生地"})
	if _, ok := byTitle["a1"]; ok {
		t.Fatal("a1 should match the title filter")
	}
	if _, ok := byTitle["b1"]; !ok {
		t.Fatal("b1 should be rejected by the title filter")
	}
	// A type mismatch never matches, mirroring the mock retriever.
	mismatch := index.RejectedKeys(map[string]any{"document_id": 42})
	if len(mismatch) != 13 {
		t.Fatalf("type-mismatched filter rejected %d passages, want 13", len(mismatch))
	}
}
