package state

import (
	"context"
	"encoding/base64"
	"math"
	"strings"
	"testing"
)

type artifactOptions struct {
	namespace  string
	documentID string
	version    *string
	kind       string
	name       string
	mimeType   string
	content    []byte
	expiresAt  *float64
}

func defaultArtifactOptions() artifactOptions {
	version := "v1"
	return artifactOptions{
		namespace:  "alpha",
		documentID: "doc-a",
		version:    &version,
		kind:       "source",
		name:       "report.pdf",
		mimeType:   "application/pdf",
		content:    []byte("%PDF-1.7\x00\xff"),
	}
}

func registerArtifact(t *testing.T, registry *ArtifactRegistry, options artifactOptions) (Artifact, error) {
	t.Helper()
	return registry.Register(context.Background(), options.namespace, options.documentID, options.version,
		options.kind, options.name, options.mimeType, options.content, options.expiresAt)
}

func newTestArtifactRegistry(t *testing.T, stores *Stores, ttl float64, clock func() float64, maxBytes *int64) *ArtifactRegistry {
	t.Helper()
	registry, err := NewArtifactRegistry(stores, ttl, clock, maxBytes)
	if err != nil {
		t.Fatalf("NewArtifactRegistry() error = %v", err)
	}
	return registry
}

func fixedClock(value float64) func() float64 { return func() float64 { return value } }

func TestArtifactStableUUIDAndRedactedMetadata(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, fixedClock(100), int64Pointer(DefaultArtifactMaxBytes))
	first, err := registerArtifact(t, registry, defaultArtifactOptions())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if !canonicalUUID(first.ArtifactID) {
		t.Fatalf("artifact id %q is not a canonical UUID", first.ArtifactID)
	}
	// uuid.UUID(...).version == 5
	if first.ArtifactID[14] != '5' {
		t.Fatalf("artifact id %q is not a version 5 UUID", first.ArtifactID)
	}
	second, err := registerArtifact(t, newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, fixedClock(100), int64Pointer(DefaultArtifactMaxBytes)), defaultArtifactOptions())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if first.ArtifactID != second.ArtifactID || first.Name != second.Name ||
		first.MimeType != second.MimeType || first.ExpiresAt != second.ExpiresAt ||
		(first.Version == nil) != (second.Version == nil) ||
		(first.Version != nil && *first.Version != *second.Version) {
		t.Fatalf("re-registration changed metadata: %+v vs %+v", first, second)
	}
	if first.ResourceURI != "rag://artifacts/alpha/"+first.ArtifactID {
		t.Fatalf("resource_uri = %q", first.ResourceURI)
	}
	if first.ExpiresAt != 3700 {
		t.Fatalf("expires_at = %v, want 3700", first.ExpiresAt)
	}
	artifact, content, err := registry.Read(context.Background(), "alpha", first.ArtifactID)
	if err != nil || string(content) != "%PDF-1.7\x00\xff" {
		t.Fatalf("Read() = %+v, %q, %v", artifact, content, err)
	}
	var count int
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM artifact_snapshots").Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot rows = %d, %v; want 1", count, err)
	}
}

func TestArtifactIdentityTracksSnapshotContent(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil)
	base, err := registerArtifact(t, registry, defaultArtifactOptions())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	otherVersion := "v2"
	changes := map[string]artifactOptions{
		"namespace":   func() artifactOptions { o := defaultArtifactOptions(); o.namespace = "Alpha"; return o }(),
		"document_id": func() artifactOptions { o := defaultArtifactOptions(); o.documentID = "doc-b"; return o }(),
		"version":     func() artifactOptions { o := defaultArtifactOptions(); o.version = &otherVersion; return o }(),
		"no_version":  func() artifactOptions { o := defaultArtifactOptions(); o.version = nil; return o }(),
		"kind":        func() artifactOptions { o := defaultArtifactOptions(); o.kind = "preview"; return o }(),
		"name":        func() artifactOptions { o := defaultArtifactOptions(); o.name = "another.pdf"; return o }(),
		"content":     func() artifactOptions { o := defaultArtifactOptions(); o.content = []byte("other bytes"); return o }(),
	}
	for name, options := range changes {
		t.Run(name, func(t *testing.T) {
			// Other namespaces/versions must not collide with the original batch.
			other, err := registerArtifact(t, newTestArtifactRegistry(t, newTestStores(t), DefaultArtifactTTLSeconds, nil, nil), options)
			if err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			if other.ArtifactID == base.ArtifactID {
				t.Fatalf("identity did not change for %s", name)
			}
		})
	}
}

func TestArtifactUnavailableIsIndistinguishable(t *testing.T) {
	stores := newTestStores(t)
	now := 100.0
	registry := newTestArtifactRegistry(t, stores, 2, func() float64 { return now }, nil)
	artifact, err := registerArtifact(t, registry, defaultArtifactOptions())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	messages := make([]string, 0, 3)
	for _, target := range []struct {
		namespace string
		id        string
	}{
		{"other", artifact.ArtifactID},
		{"Alpha", artifact.ArtifactID},
		{"alpha", "not-a-uuid"},
	} {
		_, _, err := registry.Read(context.Background(), target.namespace, target.id)
		unavailable := mustBeError[*ArtifactUnavailable](t, err)
		messages = append(messages, unavailable.Message)
	}
	now = 102
	_, _, err = registry.Read(context.Background(), "alpha", artifact.ArtifactID)
	unavailable := mustBeError[*ArtifactUnavailable](t, err)
	messages = append(messages, unavailable.Message)
	for _, message := range messages {
		if message != "Artifact is unavailable" {
			t.Fatalf("unavailable messages = %v", messages)
		}
	}
}

func TestArtifactNoncanonicalAndInvalidReadInputs(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil)
	artifact, err := registerArtifact(t, registry, defaultArtifactOptions())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	for _, invalid := range []string{
		strings.ToUpper(artifact.ArtifactID),
		strings.ReplaceAll(artifact.ArtifactID, "-", ""),
		"{" + artifact.ArtifactID + "}",
		"urn:uuid:" + artifact.ArtifactID,
		artifact.ArtifactID + " ",
	} {
		_, _, err := registry.Read(context.Background(), "alpha", invalid)
		mustBeError[*ArtifactUnavailable](t, err)
	}
	for _, namespace := range []string{"", strings.Repeat("x", 65)} {
		_, _, err := registry.Read(context.Background(), namespace, artifact.ArtifactID)
		mustBeError[*ArtifactUnavailable](t, err)
	}
}

func TestArtifactRenewalIsMonotonic(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, fixedClock(100), nil)
	original := defaultArtifactOptions()
	expiresAt := 200.0
	original.expiresAt = &expiresAt
	first, err := registerArtifact(t, registry, original)
	if err != nil {
		t.Fatalf("Register(original) error = %v", err)
	}
	renewed := defaultArtifactOptions()
	renewed.expiresAt = float64Pointer(300)
	renewed.mimeType = "application/octet-stream"
	renewedArtifact, err := registerArtifact(t, registry, renewed)
	if err != nil {
		t.Fatalf("Register(renewed) error = %v", err)
	}
	shortened := defaultArtifactOptions()
	shortened.expiresAt = float64Pointer(150)
	shortenedArtifact, err := registerArtifact(t, registry, shortened)
	if err != nil {
		t.Fatalf("Register(shortened) error = %v", err)
	}
	if first.ArtifactID != renewedArtifact.ArtifactID || first.ArtifactID != shortenedArtifact.ArtifactID {
		t.Fatal("renewal changed the artifact id")
	}
	if renewedArtifact.ExpiresAt != 300 || shortenedArtifact.ExpiresAt != 300 {
		t.Fatalf("expiries = %v, %v; want monotonic 300", renewedArtifact.ExpiresAt, shortenedArtifact.ExpiresAt)
	}
	if renewedArtifact.MimeType != first.MimeType {
		t.Fatalf("mime type changed on renewal: %q vs %q", renewedArtifact.MimeType, first.MimeType)
	}
	var createdAt float64
	var content []byte
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT created_at, content FROM artifact_snapshots").Scan(&createdAt, &content); err != nil {
		t.Fatalf("row query error = %v", err)
	}
	if createdAt != 100 || string(content) != "%PDF-1.7\x00\xff" {
		t.Fatalf("created_at/content = %v, %q", createdAt, content)
	}
}

func TestArtifactExpiredIdentityReregistration(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "purged"}[purge], func(t *testing.T) {
			stores := newTestStores(t)
			now := 100.0
			registry := newTestArtifactRegistry(t, stores, 10, func() float64 { return now }, nil)
			original, err := registerArtifact(t, registry, defaultArtifactOptions())
			if err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			now = original.ExpiresAt
			_, _, err = registry.Read(context.Background(), "alpha", original.ArtifactID)
			mustBeError[*ArtifactUnavailable](t, err)
			if purge {
				removed, err := registry.PurgeExpired(context.Background(), 100)
				if err != nil || removed != 1 {
					t.Fatalf("PurgeExpired() = %d, %v; want 1", removed, err)
				}
			}
			renewed, err := registerArtifact(t, registry, defaultArtifactOptions())
			if err != nil {
				t.Fatalf("re-Register() error = %v", err)
			}
			if renewed.ArtifactID != original.ArtifactID {
				t.Fatalf("renewed id %q != original %q", renewed.ArtifactID, original.ArtifactID)
			}
			if renewed.ExpiresAt != 120 {
				t.Fatalf("renewed expires_at = %v, want 120", renewed.ExpiresAt)
			}
			_, content, err := registry.Read(context.Background(), "alpha", original.ArtifactID)
			if err != nil || string(content) != "%PDF-1.7\x00\xff" {
				t.Fatalf("Read() after renewal = %q, %v", content, err)
			}
			var createdAt float64
			if err := stores.DB.QueryRowContext(context.Background(),
				"SELECT created_at FROM artifact_snapshots").Scan(&createdAt); err != nil {
				t.Fatalf("created_at query error = %v", err)
			}
			expected := 100.0
			if purge {
				expected = 110
			}
			if createdAt != expected {
				t.Fatalf("created_at = %v, want %v", createdAt, expected)
			}
		})
	}
}

func TestArtifactSurvivesDocumentAndBlobDeletion(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Documents.Upsert(ctx, "alpha", DocumentRecord{
		Name: "report.pdf", DocumentID: "doc-a", Collection: "c",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	content := []byte("%PDF-1.7\x00\xff")
	if err := stores.Blobs.Put(ctx, "alpha", SourceBucket, "report.pdf", content, stringPointer("doc-a")); err != nil {
		t.Fatalf("Blobs.Put() error = %v", err)
	}
	if err := stores.Blobs.Put(ctx, "alpha", PDFAssetBucket, "doc-a/page.png", []byte("page"), stringPointer("doc-a")); err != nil {
		t.Fatalf("Blobs.Put(page) error = %v", err)
	}
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil)
	options := defaultArtifactOptions()
	options.content = content
	artifact, err := registerArtifact(t, registry, options)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := stores.Documents.Delete(ctx, "alpha", "report.pdf"); err != nil {
		t.Fatalf("Documents.Delete() error = %v", err)
	}
	if err := stores.Blobs.DeleteDocument(ctx, "alpha", SourceBucket, "doc-a"); err != nil {
		t.Fatalf("DeleteDocument(source) error = %v", err)
	}
	if err := stores.Blobs.DeleteDocument(ctx, "alpha", PDFAssetBucket, "doc-a"); err != nil {
		t.Fatalf("DeleteDocument(pdf) error = %v", err)
	}
	_, stored, err := registry.Read(ctx, "alpha", artifact.ArtifactID)
	if err != nil || string(stored) != string(content) {
		t.Fatalf("Read() after cleanup = %q, %v", stored, err)
	}
}

func TestArtifactBinaryChunks(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil)
	content := make([]byte, 0, 256*9000)
	for index := 0; index < 9000; index++ {
		for value := 0; value < 256; value++ {
			content = append(content, byte(value))
		}
	}
	options := defaultArtifactOptions()
	options.content = content
	artifact, err := registerArtifact(t, registry, options)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	ctx := context.Background()
	joined := make([]byte, 0, len(content))
	offset := 0
	for {
		chunk, err := registry.Chunk(ctx, "alpha", artifact.ArtifactID, offset, 1024*1024)
		if err != nil {
			t.Fatalf("Chunk(offset=%d) error = %v", offset, err)
		}
		if chunk.Encoding != "base64" || chunk.TotalBytes != len(content) || chunk.Offset != offset ||
			chunk.Name != artifact.Name || chunk.MimeType != artifact.MimeType || chunk.ExpiresAt != artifact.ExpiresAt {
			t.Fatalf("unexpected chunk: %+v", chunk)
		}
		data, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil || len(data) > 1024*1024 {
			t.Fatalf("chunk data invalid: len=%d err=%v", len(data), err)
		}
		joined = append(joined, data...)
		if chunk.NextOffset == nil {
			break
		}
		offset = *chunk.NextOffset
	}
	if string(joined) != string(content) {
		t.Fatal("reassembled chunks differ from the original content")
	}
	defaultChunk, err := registry.Chunk(ctx, "alpha", artifact.ArtifactID, 0, 65536)
	if err != nil || len(defaultChunk.Data) == 0 {
		t.Fatalf("default chunk = %+v, %v", defaultChunk, err)
	}
	eof, err := registry.Chunk(ctx, "alpha", artifact.ArtifactID, len(content), 65536)
	if err != nil || eof.Data != "" || eof.NextOffset != nil {
		t.Fatalf("EOF chunk = %+v, %v", eof, err)
	}
	_, err = registry.Chunk(ctx, "alpha", artifact.ArtifactID, len(content)+1, 65536)
	if valueError := mustBeError[*ValueError](t, err); !strings.Contains(valueError.Message, "offset") {
		t.Fatalf("offset panic message = %q", valueError.Message)
	}
}

func TestArtifactInvalidChunkRangesRejectedBeforeLookup(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil)
	for _, testCase := range []struct {
		offset int
		length int
	}{
		{-1, 1}, {0, 0}, {0, -1}, {0, 1048577},
	} {
		if _, err := registry.Chunk(context.Background(), "alpha", "missing", testCase.offset, testCase.length); err == nil {
			t.Fatalf("Chunk(%d, %d) succeeded", testCase.offset, testCase.length)
		}
	}
}

func TestArtifactEmptyContentAndURIEscaping(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil)
	options := defaultArtifactOptions()
	options.namespace = "team/a b"
	options.version = nil
	options.content = []byte{}
	artifact, err := registerArtifact(t, registry, options)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if !strings.HasPrefix(artifact.ResourceURI, "rag://artifacts/team%2Fa%20b/") {
		t.Fatalf("resource_uri = %q", artifact.ResourceURI)
	}
	chunk, err := registry.Chunk(context.Background(), "team/a b", artifact.ArtifactID, 0, 65536)
	if err != nil || chunk.TotalBytes != 0 || chunk.NextOffset != nil {
		t.Fatalf("chunk = %+v, %v", chunk, err)
	}
	if _, err := registry.Chunk(context.Background(), "team/a b", artifact.ArtifactID, 1, 65536); err == nil {
		t.Fatal("chunk offset past an empty artifact succeeded")
	}
}

func TestArtifactSizePolicy(t *testing.T) {
	stores := newTestStores(t)
	limited := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, int64Pointer(3))
	options := defaultArtifactOptions()
	options.content = []byte("four")
	if _, err := registerArtifact(t, limited, options); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized artifact error = %v", err)
	}
	options.content = []byte("123")
	if _, err := registerArtifact(t, limited, options); err != nil {
		t.Fatalf("limit-sized artifact error = %v", err)
	}
	options.content = []byte("1234")
	if _, err := registerArtifact(t, newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil), options); err != nil {
		t.Fatalf("unlimited registry error = %v", err)
	}
	if stores.Artifacts.MaxBytes == nil || *stores.Artifacts.MaxBytes != DefaultArtifactMaxBytes {
		t.Fatalf("default registry MaxBytes = %v", stores.Artifacts.MaxBytes)
	}
}

func TestArtifactInvalidRegistryPolicy(t *testing.T) {
	stores := newTestStores(t)
	for name, testCase := range map[string]struct {
		ttl      float64
		maxBytes *int64
	}{
		"ttl zero":     {ttl: 0},
		"ttl negative": {ttl: -1},
		"ttl nan":      {ttl: math.NaN()},
		"ttl inf":      {ttl: math.Inf(1)},
		"bytes":        {ttl: 1, maxBytes: int64Pointer(-1)},
	} {
		if _, err := NewArtifactRegistry(stores, testCase.ttl, nil, testCase.maxBytes); err == nil {
			t.Fatalf("%s: NewArtifactRegistry() succeeded", name)
		}
	}
}

func TestArtifactInvalidRegistrationDoesNotWrite(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, nil, nil)
	badNumber := math.NaN()
	for name, options := range map[string]artifactOptions{
		"expires nan": func() artifactOptions { o := defaultArtifactOptions(); o.expiresAt = &badNumber; return o }(),
		"expires inf": func() artifactOptions {
			o := defaultArtifactOptions()
			o.expiresAt = float64Pointer(math.Inf(1))
			return o
		}(),
		"empty name": func() artifactOptions { o := defaultArtifactOptions(); o.name = ""; return o }(),
		"namespace":  func() artifactOptions { o := defaultArtifactOptions(); o.namespace = strings.Repeat("x", 65); return o }(),
		"long version": func() artifactOptions {
			o := defaultArtifactOptions()
			v := strings.Repeat("x", 65)
			o.version = &v
			return o
		}(),
		"empty document": func() artifactOptions { o := defaultArtifactOptions(); o.documentID = ""; return o }(),
	} {
		if _, err := registerArtifact(t, registry, options); err == nil {
			t.Fatalf("%s: Register() succeeded", name)
		}
	}
	var count int
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM artifact_snapshots").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid registrations wrote %d rows, %v", count, err)
	}
}

func TestArtifactInvalidClockCannotBypassExpiry(t *testing.T) {
	stores := newTestStores(t)
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, fixedClock(100), nil)
	artifact, err := registerArtifact(t, registry, defaultArtifactOptions())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	for _, badClock := range []float64{math.NaN(), math.Inf(1)} {
		registry.Clock = fixedClock(badClock)
		_, _, err := registry.Read(context.Background(), "alpha", artifact.ArtifactID)
		if valueError := mustBeError[*ValueError](t, err); !strings.Contains(valueError.Message, "clock") {
			t.Fatalf("read clock message = %q", valueError.Message)
		}
		if _, err := registry.PurgeExpired(context.Background(), 10); err == nil {
			t.Fatal("PurgeExpired() with a bad clock succeeded")
		}
	}
	fresh := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, fixedClock(100), nil)
	if _, _, err := fresh.Read(context.Background(), "alpha", artifact.ArtifactID); err != nil {
		t.Fatalf("live artifact unreadable: %v", err)
	}
}

func TestArtifactPurgeIsBoundedAndExpiryInclusive(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	registry := newTestArtifactRegistry(t, stores, DefaultArtifactTTLSeconds, fixedClock(100), nil)
	for index := 0; index < 5; index++ {
		options := defaultArtifactOptions()
		options.name = strings.Repeat("n", index+1) + ".pdf"
		options.expiresAt = float64Pointer(100)
		if _, err := registerArtifact(t, registry, options); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}
	liveOptions := defaultArtifactOptions()
	liveOptions.name = "live.pdf"
	liveOptions.expiresAt = float64Pointer(101)
	live, err := registerArtifact(t, registry, liveOptions)
	if err != nil {
		t.Fatalf("Register(live) error = %v", err)
	}
	otherOptions := defaultArtifactOptions()
	otherOptions.namespace = "other"
	otherOptions.name = "live.pdf"
	otherOptions.expiresAt = float64Pointer(1000)
	other, err := registerArtifact(t, registry, otherOptions)
	if err != nil {
		t.Fatalf("Register(other) error = %v", err)
	}
	for _, expected := range []int{2, 2, 1, 0} {
		removed, err := registry.PurgeExpired(ctx, 2)
		if err != nil || removed != expected {
			t.Fatalf("PurgeExpired() = %d, %v; want %d", removed, err, expected)
		}
	}
	if _, _, err := registry.Read(ctx, "alpha", live.ArtifactID); err != nil {
		t.Fatalf("live artifact was purged: %v", err)
	}
	if _, _, err := registry.Read(ctx, "other", other.ArtifactID); err != nil {
		t.Fatalf("other namespace artifact was purged: %v", err)
	}
	for _, limit := range []int{0, -1} {
		if _, err := registry.PurgeExpired(ctx, limit); err == nil {
			t.Fatalf("PurgeExpired(%d) succeeded", limit)
		}
	}
}

func TestArtifactMySQLUsesNativeGreatestUpsert(t *testing.T) {
	statement := artifactUpsertSQL()
	if !strings.Contains(statement, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("MySQL statement = %q", statement)
	}
	if !strings.Contains(statement, "GREATEST(artifact_snapshots.expires_at, VALUES(expires_at))") {
		t.Fatalf("MySQL statement does not use GREATEST: %q", statement)
	}
}

func int64Pointer(value int64) *int64       { return &value }
func float64Pointer(value float64) *float64 { return &value }
