package state

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestAPIKeyLifecycleAndPortableEncryption ports
// tests/test_state_store.py::test_api_key_lifecycle_and_portable_encryption.
func TestAPIKeyLifecycleAndPortableEncryption(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	created, err := stores.APIKeys.Create(ctx, "app", []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !strings.HasPrefix(created.Key, "prk_") {
		t.Fatalf("secret %q does not have the prk_ prefix", created.Key)
	}
	// secrets.token_urlsafe(32) is 43 characters, so "prk_" + token is 47.
	if len(created.Key) != 47 || created.Prefix != created.Key[:12] {
		t.Fatalf("unexpected secret shape: key=%q prefix=%q", created.Key, created.Prefix)
	}
	if !created.Enabled || !created.Exportable {
		t.Fatalf("unexpected public flags: %+v", created.APIKey)
	}
	namespaces, err := stores.APIKeys.NamespacesForKey(ctx, created.Key)
	if err != nil {
		t.Fatalf("NamespacesForKey() error = %v", err)
	}
	if len(namespaces) != 2 || namespaces[0] != "alpha" || namespaces[1] != "beta" {
		t.Fatalf("namespaces = %v", namespaces)
	}
	secret, err := stores.APIKeys.SecretForNamespace(ctx, created.ID, "alpha")
	if err != nil {
		t.Fatalf("SecretForNamespace() error = %v", err)
	}
	if secret != created.Key {
		t.Fatalf("SecretForNamespace() = %q, want %q", secret, created.Key)
	}

	list, err := stores.APIKeys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID || list[0].Exportable != true {
		t.Fatalf("unexpected list: %+v", list)
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte(created.Key)) {
		t.Fatal("List() exposed the plaintext secret")
	}

	_, err = stores.APIKeys.SecretForNamespace(ctx, created.ID, "other")
	permission := mustBeError[*PermissionError](t, err)
	if permission.Message != "API Key 已停用或未获授权访问此 Namespace" {
		t.Fatalf("permission message = %q", permission.Message)
	}

	var protected *string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT protected_key FROM api_keys WHERE id = ?", created.ID).Scan(&protected); err != nil {
		t.Fatalf("reading the stored secret: %v", err)
	}
	if protected == nil {
		t.Fatal("the protected secret was not stored")
	}
	if strings.Contains(*protected, created.Key) {
		t.Fatal("the plaintext secret is stored in the database")
	}
	if strings.Contains(*protected, "dpapi:") {
		t.Fatal("the database contains a legacy dpapi value")
	}

	updated, err := stores.APIKeys.Update(ctx, created.ID, "renamed", []string{"beta"}, false)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated == nil || updated.Name != "renamed" || updated.Enabled {
		t.Fatalf("unexpected update result: %+v", updated)
	}
	_, err = stores.APIKeys.NamespacesForKey(ctx, created.Key)
	disabled := mustBeError[*PermissionError](t, err)
	if disabled.Message != "访问 API Key 已停用" {
		t.Fatalf("disabled message = %q", disabled.Message)
	}

	deleted, err := stores.APIKeys.Delete(ctx, created.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v; want true", deleted, err)
	}
	deleted, err = stores.APIKeys.Delete(ctx, created.ID)
	if err != nil || deleted {
		t.Fatalf("second Delete() = %v, %v; want false", deleted, err)
	}
	_, err = stores.APIKeys.NamespacesForKey(ctx, created.Key)
	lookup := mustBeError[*LookupError](t, err)
	if lookup.Message != "访问 API Key 在当前服务中不存在或已删除" {
		t.Fatalf("lookup message = %q", lookup.Message)
	}
}

func TestAPIKeyStoresDigestAndProtectedValue(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	created, err := stores.APIKeys.Create(ctx, "app", []string{"alpha"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	var digest string
	var protected string
	var enabled bool
	err = stores.DB.QueryRowContext(ctx,
		"SELECT digest, protected_key, enabled FROM api_keys WHERE id = ?", created.ID).
		Scan(&digest, &protected, &enabled)
	if err != nil {
		t.Fatalf("row query error = %v", err)
	}
	if digest != sha256Hex(created.Key) {
		t.Fatalf("digest = %q, want sha256(secret)", digest)
	}
	if !strings.HasPrefix(protected, EncryptionPrefix) {
		t.Fatalf("protected_key = %q, want enc:v1:", protected)
	}
	if protected == created.Key {
		t.Fatal("protected_key stored the plaintext secret")
	}
	decrypted, err := Unprotect(protected, stores.Settings.EncryptionKey)
	if err != nil || decrypted != created.Key {
		t.Fatalf("Unprotect(protected) = %q, %v", decrypted, err)
	}
	if !enabled {
		t.Fatal("new API key is not enabled")
	}
}

// TestAPIKeyDigestOnlyRowsArePreservedButNotExportable ports the repository
// behavior behind tests/test_api_keys.py::test_digest_only_keys_are_preserved_but_cannot_be_exported.
func TestAPIKeyDigestOnlyRowsArePreservedButNotExportable(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	secret := "prk_legacy_test_key"
	err := stores.APIKeys.ImportRow(ctx, APIKeyImport{
		ID:           "old",
		Name:         "Old key",
		Prefix:       secret[:12],
		Digest:       sha256Hex(secret),
		Namespaces:   []string{"alpha"},
		Enabled:      true,
		CreatedAt:    "2026-09-13",
		ProtectedKey: nil,
	})
	if err != nil {
		t.Fatalf("ImportRow() error = %v", err)
	}
	namespaces, err := stores.APIKeys.NamespacesForKey(ctx, secret)
	if err != nil || len(namespaces) != 1 || namespaces[0] != "alpha" {
		t.Fatalf("NamespacesForKey() = %v, %v", namespaces, err)
	}
	list, err := stores.APIKeys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Exportable {
		t.Fatalf("digest-only key is exportable: %+v", list)
	}
	_, err = stores.APIKeys.SecretForNamespace(ctx, "old", "alpha")
	valueError := mustBeError[*ValueError](t, err)
	if valueError.Message != "该密钥仅保存摘要，无法显示原值；请在 API Key 页面重新创建" {
		t.Fatalf("message = %q", valueError.Message)
	}
	// Importing the same id updates in place instead of inserting a duplicate.
	if err := stores.APIKeys.ImportRow(ctx, APIKeyImport{
		ID: "old", Name: "Renamed", Prefix: secret[:12], Digest: sha256Hex(secret),
		Namespaces: []string{"beta"}, Enabled: true, CreatedAt: "2026-09-13", ProtectedKey: nil,
	}); err != nil {
		t.Fatalf("second ImportRow() error = %v", err)
	}
	var count int
	if err := stores.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_keys").Scan(&count); err != nil {
		t.Fatalf("count error = %v", err)
	}
	if count != 1 {
		t.Fatalf("api_keys rows = %d, want 1", count)
	}
	namespaces, err = stores.APIKeys.NamespacesForKey(ctx, secret)
	if err != nil || len(namespaces) != 1 || namespaces[0] != "beta" {
		t.Fatalf("namespaces after import update = %v, %v", namespaces, err)
	}
	// Missing and disabled keys keep the Python error categories.
	_, err = stores.APIKeys.SecretForNamespace(ctx, "missing", "alpha")
	lookup := mustBeError[*LookupError](t, err)
	if lookup.Message != "API Key 不存在" {
		t.Fatalf("missing message = %q", lookup.Message)
	}
	if err := stores.APIKeys.ImportRow(ctx, APIKeyImport{
		ID: "disabled", Name: "Disabled", Prefix: secret[:12], Digest: sha256Hex("prk_other"),
		Namespaces: []string{"alpha"}, Enabled: false, CreatedAt: "2026-09-13",
	}); err != nil {
		t.Fatalf("ImportRow(disabled) error = %v", err)
	}
	_, err = stores.APIKeys.SecretForNamespace(ctx, "disabled", "alpha")
	if permission := mustBeError[*PermissionError](t, err); permission.Message != "API Key 已停用或未获授权访问此 Namespace" {
		t.Fatalf("disabled message = %q", permission.Message)
	}
	_, err = stores.APIKeys.NamespacesForKey(ctx, "prk_other")
	if permission := mustBeError[*PermissionError](t, err); permission.Message != "访问 API Key 已停用" {
		t.Fatalf("disabled lookup message = %q", permission.Message)
	}
}

func TestAPIKeyImportReencryptsWithCurrentKey(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	created, err := stores.APIKeys.Create(ctx, "app", []string{"alpha"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	var protected string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT protected_key FROM api_keys WHERE id = ?", created.ID).Scan(&protected); err != nil {
		t.Fatalf("protected_key query error = %v", err)
	}
	if err := stores.APIKeys.ImportRow(ctx, APIKeyImport{
		ID: created.ID, Name: created.Name, Prefix: created.Prefix,
		Digest: sha256Hex(created.Key), Namespaces: created.Namespaces, Enabled: true,
		CreatedAt:    created.CreatedAt,
		ProtectedKey: &protected,
	}); err != nil {
		t.Fatalf("ImportRow() error = %v", err)
	}
	var reprotected string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT protected_key FROM api_keys WHERE id = ?", created.ID).Scan(&reprotected); err != nil {
		t.Fatalf("protected_key query error = %v", err)
	}
	if reprotected == protected {
		t.Fatal("ImportRow() reused the exact ciphertext instead of re-protecting")
	}
	secret, err := stores.APIKeys.SecretForNamespace(ctx, created.ID, "alpha")
	if err != nil || secret != created.Key {
		t.Fatalf("SecretForNamespace() = %q, %v", secret, err)
	}
}

func TestAPIKeyUpdateUnknownReturnsNil(t *testing.T) {
	stores := newTestStores(t)
	updated, err := stores.APIKeys.Update(context.Background(), "missing", "name", []string{"alpha"}, true)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated != nil {
		t.Fatalf("Update() = %+v, want nil", updated)
	}
}

func TestAPIKeyListOrdering(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	first, err := stores.APIKeys.Create(ctx, "first", []string{"alpha"})
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, err := stores.APIKeys.Create(ctx, "second", []string{"alpha"})
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx, "UPDATE api_keys SET created_at = ? WHERE id = ?",
		"2026-01-01T00:00:00.000000+00:00", first.ID); err != nil {
		t.Fatalf("created_at update error = %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx, "UPDATE api_keys SET created_at = ? WHERE id = ?",
		"2026-02-01T00:00:00.000000+00:00", second.ID); err != nil {
		t.Fatalf("created_at update error = %v", err)
	}
	list, err := stores.APIKeys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list order = %v", []string{list[0].ID, list[1].ID})
	}
}
