package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFeishuConnectionRoundTripProtectsTokens(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	connection := &FeishuConnection{
		UserID: "u1", OpenID: "ou_1", UnionID: "on_1", Name: "张三",
		AvatarURL: "https://avatar", AccessToken: "u-secret", RefreshToken: "r-secret",
		AccessExpiresAt: 100, RefreshExpiresAt: 200, Scope: "drive:drive:readonly",
	}
	if err := stores.FeishuConnections.Upsert(ctx, connection); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	stored, err := stores.FeishuConnections.Get(ctx, "u1")
	if err != nil || stored == nil {
		t.Fatalf("Get() = %+v, %v", stored, err)
	}
	if stored.AccessToken != "u-secret" || stored.RefreshToken != "r-secret" {
		t.Fatalf("decrypted tokens = %q / %q", stored.AccessToken, stored.RefreshToken)
	}
	if stored.Name != "张三" || stored.OpenID != "ou_1" || stored.UnionID != "on_1" {
		t.Fatalf("stored account = %+v", stored)
	}
	var access, refresh string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT access_token, refresh_token FROM feishu_connections WHERE user_id = ?", "u1").
		Scan(&access, &refresh); err != nil {
		t.Fatalf("raw token query: %v", err)
	}
	if !strings.HasPrefix(access, EncryptionPrefix) || !strings.HasPrefix(refresh, EncryptionPrefix) {
		t.Fatalf("raw tokens are not protected: %q / %q", access, refresh)
	}
	if strings.Contains(access, "u-secret") || strings.Contains(refresh, "r-secret") {
		t.Fatal("raw tokens contain plaintext")
	}

	// Upsert replaces the tokens and preserves created_at.
	createdAt := stored.CreatedAt
	connection.AccessToken = "u-secret-2"
	connection.RefreshToken = "r-secret-2"
	if err := stores.FeishuConnections.Upsert(ctx, connection); err != nil {
		t.Fatalf("second Upsert() error = %v", err)
	}
	updated, err := stores.FeishuConnections.Get(ctx, "u1")
	if err != nil || updated == nil {
		t.Fatalf("second Get() = %+v, %v", updated, err)
	}
	if updated.AccessToken != "u-secret-2" || updated.RefreshToken != "r-secret-2" {
		t.Fatalf("updated tokens = %q / %q", updated.AccessToken, updated.RefreshToken)
	}
	if updated.CreatedAt != createdAt {
		t.Fatalf("created_at = %q, want %q", updated.CreatedAt, createdAt)
	}

	deleted, err := stores.FeishuConnections.Delete(ctx, "u1")
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v", deleted, err)
	}
	missing, err := stores.FeishuConnections.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("Get() after delete error = %v", err)
	}
	if missing != nil {
		t.Fatalf("connection still present: %+v", missing)
	}
}

func TestFeishuOAuthStateIsSingleUse(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := time.Now().Unix()
	state := &FeishuOAuthState{
		StateDigest: "digest-1", UserID: "u1",
		RedirectURI: "https://rag.example.com/cb", ExpiresAt: now + 60, CreatedAt: now,
	}
	if err := stores.FeishuOAuthStates.Create(ctx, state); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	consumed, err := stores.FeishuOAuthStates.Consume(ctx, "digest-1", "u1", now)
	if err != nil || consumed == nil {
		t.Fatalf("Consume() = %+v, %v", consumed, err)
	}
	if consumed.RedirectURI != "https://rag.example.com/cb" {
		t.Fatalf("redirect URI = %q", consumed.RedirectURI)
	}
	if second, err := stores.FeishuOAuthStates.Consume(ctx, "digest-1", "u1", now); err != nil || second != nil {
		t.Fatalf("second Consume() = %+v, %v", second, err)
	}
	// A different account cannot consume this state even before it is used.
	if err := stores.FeishuOAuthStates.Create(ctx, &FeishuOAuthState{
		StateDigest: "digest-2", UserID: "u2",
		RedirectURI: "https://rag.example.com/cb", ExpiresAt: now + 60, CreatedAt: now,
	}); err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
	if other, err := stores.FeishuOAuthStates.Consume(ctx, "digest-2", "u1", now); err != nil || other != nil {
		t.Fatalf("other-user Consume() = %+v, %v", other, err)
	}
	if own, err := stores.FeishuOAuthStates.Consume(ctx, "digest-2", "u2", now); err != nil || own == nil {
		t.Fatalf("owner Consume() = %+v, %v", own, err)
	}
	// An expired state cannot be consumed.
	if err := stores.FeishuOAuthStates.Create(ctx, &FeishuOAuthState{
		StateDigest: "digest-3", UserID: "u1",
		RedirectURI: "https://rag.example.com/cb", ExpiresAt: now + 60, CreatedAt: now,
	}); err != nil {
		t.Fatalf("expired Create() error = %v", err)
	}
	if expired, err := stores.FeishuOAuthStates.Consume(ctx, "digest-3", "u1", now+120); err != nil || expired != nil {
		t.Fatalf("expired Consume() = %+v, %v", expired, err)
	}
}
