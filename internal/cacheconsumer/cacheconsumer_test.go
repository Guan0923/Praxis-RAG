package cacheconsumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// fakeRefresher records the keys it was asked to refresh and can fail.
type fakeRefresher struct {
	revalidated [][]state.CacheKey
	recoveries  int
	fail        error
}

func (f *fakeRefresher) Revalidate(_ context.Context, keys []state.CacheKey) error {
	f.revalidated = append(f.revalidated, keys)
	return f.fail
}

func (f *fakeRefresher) RefreshDirty(_ context.Context, _ int) error {
	f.recoveries++
	return f.fail
}

func change(table, op string, before, after map[string]any) Change {
	return Change{Table: table, Op: op, Before: before, After: after}
}

func ignorableDecoder() Decoder {
	return func([]byte) (Change, error) { return Change{}, state.ErrIgnorableCDCChange }
}

func TestKeysForChangeMapsCachedTables(t *testing.T) {
	cases := []struct {
		name   string
		change Change
		want   []state.CacheKey
	}{
		{
			name:   "knowledge base create",
			change: change("knowledge_bases", "c", nil, map[string]any{"namespace": "manuals", "version": float64(1)}),
			want: []state.CacheKey{
				{Kind: "kb_list", Scope: ""},
				{Kind: "kb_public", Scope: "manuals"},
				{Kind: "documents", Scope: "manuals"},
			},
		},
		{
			name:   "document delete",
			change: change("documents", "d", map[string]any{"namespace": "manuals", "name": "a.pdf"}, nil),
			want: []state.CacheKey{
				{Kind: "kb_list", Scope: ""},
				{Kind: "documents", Scope: "manuals"},
			},
		},
		{
			name:   "catalog update",
			change: change("mcp_catalogs", "u", nil, map[string]any{"namespace": "manuals"}),
			want:   []state.CacheKey{{Kind: "catalog", Scope: "manuals"}},
		},
		{
			name:   "api key delete",
			change: change("api_keys", "d", map[string]any{"id": "k1", "owner_user_id": "u1"}, nil),
			want:   []state.CacheKey{{Kind: "api_keys", Scope: ""}},
		},
		{
			name:   "document head update also refreshes the documents view",
			change: change("document_heads", "u", nil, map[string]any{"namespace": "manuals", "document_id": "d1"}),
			want: []state.CacheKey{
				{Kind: "heads", Scope: "manuals"},
				{Kind: "documents", Scope: "manuals"},
			},
		},
		{
			name:   "document lease delete",
			change: change("document_leases", "d", map[string]any{"namespace": "manuals", "resource": "doc"}, nil),
			want:   []state.CacheKey{{Kind: "leases", Scope: "manuals"}},
		},
		{
			name:   "upload operation create",
			change: change("upload_operations", "c", nil, map[string]any{"namespace": "manuals", "id": "op1"}),
			want: []state.CacheKey{
				{Kind: "upload_list", Scope: "manuals"},
				{Kind: "upload_status", Scope: "op1"},
			},
		},
		{
			name:   "upload step create",
			change: change("upload_steps", "c", nil, map[string]any{"operation_id": "op1"}),
			want:   []state.CacheKey{{Kind: "upload_status", Scope: "op1"}},
		},
		{
			name:   "qualified table name",
			change: change("praxis_rag.mcp_catalogs", "c", nil, map[string]any{"namespace": "manuals"}),
			want:   []state.CacheKey{{Kind: "catalog", Scope: "manuals"}},
		},
		{
			name:   "unknown table is ignored",
			change: change("users", "u", nil, map[string]any{"id": "u1"}),
			want:   nil,
		},
		{
			name:   "missing namespace is ignored",
			change: change("documents", "u", nil, map[string]any{"name": "a.pdf"}),
			want:   nil,
		},
		{
			name:   "delete without before image is ignored",
			change: change("documents", "d", nil, nil),
			want:   nil,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := KeysForChange(test.change)
			if len(got) != len(test.want) {
				t.Fatalf("KeysForChange() = %+v, want %+v", got, test.want)
			}
			for index, key := range test.want {
				if got[index] != key {
					t.Fatalf("keys[%d] = %+v, want %+v", index, got[index], key)
				}
			}
		})
	}
}

func TestHandleRevalidatesAffectedKeys(t *testing.T) {
	refresher := &fakeRefresher{}
	processor, err := New(Options{Refresher: refresher, Decoder: ignorableDecoder()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ok := processor.Handle(context.Background(), change("documents", "u", nil,
		map[string]any{"namespace": "manuals", "name": "a.pdf", "document_id": "d1", "del_flag": false}))
	if !ok {
		t.Fatal("Handle() = false, want true")
	}
	if len(refresher.revalidated) != 1 || len(refresher.revalidated[0]) != 2 {
		t.Fatalf("revalidated = %+v", refresher.revalidated)
	}
}

func TestHandleAcknowledgesUnmappedChange(t *testing.T) {
	refresher := &fakeRefresher{}
	processor, err := New(Options{Refresher: refresher, Decoder: ignorableDecoder()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !processor.Handle(context.Background(), change("users", "u", nil, map[string]any{"id": "u1"})) {
		t.Fatal("Handle() = false, want true")
	}
	if len(refresher.revalidated) != 0 {
		t.Fatalf("revalidated = %+v", refresher.revalidated)
	}
}

func TestHandleNacksWhenStateLayerFails(t *testing.T) {
	refresher := &fakeRefresher{fail: errors.New("mysql down")}
	processor, err := New(Options{Refresher: refresher, Decoder: ignorableDecoder()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if processor.Handle(context.Background(), change("api_keys", "u", nil, map[string]any{"id": "k1"})) {
		t.Fatal("Handle() = true, want false on state failure")
	}
}

func TestMessageHandlerDecoderContract(t *testing.T) {
	refresher := &fakeRefresher{}
	var gotBody string
	processor, err := New(Options{Refresher: refresher, Decoder: func(body []byte) (Change, error) {
		gotBody = string(body)
		switch string(body) {
		case "ignore":
			return Change{}, state.ErrIgnorableCDCChange
		case "poison":
			return Change{}, errors.New("malformed")
		}
		return change("mcp_catalogs", "c", nil, map[string]any{"namespace": "manuals"}), nil
	}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	handler := processor.MessageHandler()

	if !handler(context.Background(), mq.ReceivedMessage{Body: []byte(`{"table":"mcp_catalogs"}`)}) {
		t.Fatal("valid message was nacked")
	}
	if gotBody == "" {
		t.Fatal("decoder was not called")
	}
	if !handler(context.Background(), mq.ReceivedMessage{Body: []byte("ignore")}) {
		t.Fatal("ignorable message was nacked")
	}
	if handler(context.Background(), mq.ReceivedMessage{Body: []byte("poison")}) {
		t.Fatal("malformed message was acked")
	}
}

func TestRecoverRefreshesDirtyKeys(t *testing.T) {
	refresher := &fakeRefresher{}
	processor, err := New(Options{
		Refresher:        refresher,
		Decoder:          ignorableDecoder(),
		RecoveryInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = processor.RunRecovery(ctx)
	}()
	deadline := time.After(2 * time.Second)
	for refresher.recoveries == 0 {
		select {
		case <-deadline:
			t.Fatal("RunRecovery did not run a compensation pass")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	processor.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRecovery did not stop")
	}
}

func TestDecoderJSONRowImages(t *testing.T) {
	// A decoder similar to mq.DecodeCacheChange: the payload carries before/after
	// images with locating columns only.
	decoder := func(body []byte) (Change, error) {
		var raw struct {
			Table  string         `json:"table"`
			Op     string         `json:"op"`
			Before map[string]any `json:"before"`
			After  map[string]any `json:"after"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return Change{}, err
		}
		if raw.Table == "" {
			return Change{}, state.ErrIgnorableCDCChange
		}
		return Change{Table: raw.Table, Op: raw.Op, Before: raw.Before, After: raw.After}, nil
	}
	refresher := &fakeRefresher{}
	processor, err := New(Options{Refresher: refresher, Decoder: decoder})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	body := []byte(`{"table":"knowledge_bases","op":"u","before":{"namespace":"manuals"},"after":{"namespace":"manuals"}}`)
	if !processor.MessageHandler()(context.Background(), mq.ReceivedMessage{Body: body}) {
		t.Fatal("knowledge-base update was nacked")
	}
	if len(refresher.revalidated) != 1 || len(refresher.revalidated[0]) != 3 {
		t.Fatalf("revalidated = %+v", refresher.revalidated)
	}
}

// TestKeysForChangeCrossNamespaceUpdate pins that a directly executed UPDATE
// which moves a row between scopes invalidates the old scope as well as the
// new one, instead of waiting for the cache TTL.
func TestKeysForChangeCrossNamespaceUpdate(t *testing.T) {
	got := KeysForChange(change("documents", "u",
		map[string]any{"namespace": "archive"},
		map[string]any{"namespace": "manuals"}))
	want := []state.CacheKey{
		{Kind: "kb_list", Scope: ""},
		{Kind: "documents", Scope: "manuals"},
		{Kind: "documents", Scope: "archive"},
	}
	if len(got) != len(want) {
		t.Fatalf("KeysForChange() = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("keys[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
}
