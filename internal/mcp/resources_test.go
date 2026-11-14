package mcp

import (
	"context"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/xinghe_rag/internal/resources"
)

func TestParseArtifactURI(t *testing.T) {
	artifactID := "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"
	base := "rag://artifacts/manuals/" + artifactID
	cases := []struct {
		name      string
		uri       string
		wantID    string
		wantOff   int
		wantLen   int
		wantError string
	}{
		{"defaults", base, artifactID, 0, 65536, ""},
		{"explicit", base + "?offset=7&length=10", artifactID, 7, 10, ""},
		{"reordered", base + "?length=10&offset=7", artifactID, 7, 10, ""},
		{"max length", base + "?offset=0&length=1048576", artifactID, 0, 1048576, ""},
		{"bad uuid", "rag://artifacts/manuals/not-a-uuid", "", 0, 0, "Expected a dashed UUID string"},
		{"uppercase uuid", "rag://artifacts/manuals/" + strings.ToUpper(artifactID), "", 0, 0, "Expected a dashed UUID string"},
		{"foreign namespace", "rag://artifacts/other/" + artifactID, "", 0, 0, "Artifact URI is outside this namespace or non-canonical"},
		{"encoded namespace", "rag://artifacts/man%75als/" + artifactID, "", 0, 0, "Artifact URI is outside this namespace or non-canonical"},
		{"wrong scheme", "http://artifacts/manuals/" + artifactID, "", 0, 0, "Invalid artifact URI"},
		{"wrong host", "rag://knowledge/manuals/" + artifactID, "", 0, 0, "Invalid artifact URI"},
		{"control char", base + "\n", "", 0, 0, "Invalid artifact URI"},
		{"fragment", base + "#frag", "", 0, 0, "Invalid artifact URI"},
		{"missing segment", "rag://artifacts/manuals", "", 0, 0, "Invalid artifact URI"},
		{"empty query", base + "?", "", 0, 0, "Invalid artifact chunk parameters"},
		{"field without equals", base + "?offset", "", 0, 0, "Invalid artifact chunk parameters"},
		{"unknown key", base + "?bogus=1", "", 0, 0, "Invalid artifact chunk parameters"},
		{"duplicate key", base + "?offset=1&offset=2", "", 0, 0, "Invalid artifact chunk parameters"},
		{"percent encoded value", base + "?offset=%31", "", 0, 0, "Invalid artifact chunk parameters"},
		{"leading zero", base + "?offset=00&length=1", "", 0, 0, "Artifact chunk parameters must be canonical integers"},
		{"plus sign", base + "?offset=+1&length=1", "", 0, 0, "Invalid artifact chunk parameters"},
		{"empty offset", base + "?offset=&length=1", "", 0, 0, "Invalid artifact chunk parameters"},
		{"zero length", base + "?offset=0&length=0", "", 0, 0, "length must be between 1 and 1048576 bytes"},
		{"oversized length", base + "?offset=0&length=1048577", "", 0, 0, "length must be between 1 and 1048576 bytes"},
		{"too many digits", base + "?offset=000000000000000000001", "", 0, 0, "Invalid artifact chunk parameters"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			artifactID, offset, length, err := parseArtifactURI(manualsNS, testCase.uri)
			if testCase.wantError != "" {
				if err == nil || err.Error() != testCase.wantError {
					t.Fatalf("error = %v, want %q", err, testCase.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if artifactID != testCase.wantID || offset != testCase.wantOff || length != testCase.wantLen {
				t.Fatalf("parsed = %q %d %d, want %q %d %d",
					artifactID, offset, length, testCase.wantID, testCase.wantOff, testCase.wantLen)
			}
		})
	}
}

func TestArtifactURIConstruction(t *testing.T) {
	artifactID := "3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a"
	if got := artifactURI(manualsNS, artifactID); got != "rag://artifacts/manuals/"+artifactID {
		t.Fatalf("artifactURI = %q", got)
	}
	if got := artifactURI("dot.name-1", artifactID); got != "rag://artifacts/dot.name-1/"+artifactID {
		t.Fatalf("artifactURI = %q", got)
	}
}

func TestListResourcesPagination(t *testing.T) {
	runtime := newFakeRuntime()
	for index := 0; index < 55; index++ {
		runtime.listDocuments = append(runtime.listDocuments, DocumentRecord{
			"name": "document-" + strconv.Itoa(index) + ".txt",
		})
	}
	endpoint := newTestEndpoint(t, runtime, Options{})

	first, err := endpoint.listResources(context.Background(), manualsNS, nil)
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(first.Resources) != listPageSize || first.NextCursor != "50" {
		t.Fatalf("page 1 = %d resources, cursor %q", len(first.Resources), first.NextCursor)
	}
	if first.Resources[0].MIMEType != "application/json" || first.Resources[0].Description != resourceDescription {
		t.Fatalf("resource metadata = %#v", first.Resources[0])
	}
	if first.Resources[0].URI != resources.DocumentURI(manualsNS, runtime.listDocuments[0].Name()) {
		t.Fatalf("resource uri = %q", first.Resources[0].URI)
	}
	second, err := endpoint.listResources(context.Background(), manualsNS, &sdk.ListResourcesParams{Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(second.Resources) != 5 || second.NextCursor != "" {
		t.Fatalf("page 2 = %d resources, cursor %q", len(second.Resources), second.NextCursor)
	}
	if _, err := endpoint.listResources(context.Background(), manualsNS, &sdk.ListResourcesParams{Cursor: "bad"}); err == nil {
		t.Fatalf("invalid cursor must fail")
	}
	if _, err := endpoint.listResources(context.Background(), manualsNS, &sdk.ListResourcesParams{Cursor: "56"}); err == nil {
		t.Fatalf("out-of-range cursor must fail")
	}
}

func TestListResourceTemplates(t *testing.T) {
	endpoint := newTestEndpoint(t, newFakeRuntime(), Options{})
	result, err := endpoint.listResourceTemplates(context.Background(), manualsNS, nil)
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(result.ResourceTemplates) != 2 {
		t.Fatalf("templates = %d, want 2", len(result.ResourceTemplates))
	}
	documentTemplate := result.ResourceTemplates[0]
	if documentTemplate.URITemplate != "rag://knowledge/manuals/{name}{?page,version}" ||
		documentTemplate.Name != "Document paragraph pages" || documentTemplate.MIMEType != "application/json" {
		t.Fatalf("document template = %#v", documentTemplate)
	}
	artifactTemplate := result.ResourceTemplates[1]
	if artifactTemplate.URITemplate != "rag://artifacts/{namespace}/{artifact_id}{?offset,length}" ||
		artifactTemplate.Name != "Artifact byte chunks" ||
		artifactTemplate.Description != "Read an expiring artifact UUID as a bounded JSON chunk envelope; follow next_uri." ||
		artifactTemplate.MIMEType != "application/json" {
		t.Fatalf("artifact template = %#v", artifactTemplate)
	}
}
