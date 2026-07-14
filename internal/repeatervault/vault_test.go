package repeatervault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveKeyRequiresSource(t *testing.T) {
	if _, err := DeriveKey("", ""); err != ErrNoVaultKey {
		t.Fatalf("expected ErrNoVaultKey, got %v", err)
	}
}

func TestDeriveKeyHexEnv(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	k1, err := DeriveKey(hexKey, "")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := DeriveKey(hexKey, "ignored-api-key")
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("hex env key should be deterministic and preferred over apiKey")
	}
}

func TestAddListUpdateDeleteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key, err := DeriveKey("", "test-api-key-strong-enough")
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}

	pk := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	view, err := store.Add(pk, "Hilltop RPT", "admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	if view.PublicKey != pk {
		t.Fatalf("publicKey=%q", view.PublicKey)
	}
	if !view.HasAdminPassword {
		t.Fatal("expected hasAdminPassword")
	}
	if view.Name != "Hilltop RPT" {
		t.Fatalf("name=%q", view.Name)
	}

	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list len=%d", len(list))
	}
	// Ensure ciphertext never leaks into PublicView JSON shape fields we care about.
	if list[0].ID == "" {
		t.Fatal("missing id")
	}

	plain, err := store.DecryptAdminPassword(pk)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "admin-secret" {
		t.Fatalf("decrypted=%q", plain)
	}

	// Duplicate pubkey rejected.
	if _, err := store.Add(pk, "dup", "x"); err != ErrDuplicateKey {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}

	updated, err := store.Update(view.ID, "Renamed", "new-secret", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Renamed" {
		t.Fatalf("updated name=%q", updated.Name)
	}
	plain2, err := store.DecryptAdminPassword(pk)
	if err != nil {
		t.Fatal(err)
	}
	if plain2 != "new-secret" {
		t.Fatalf("decrypted after update=%q", plain2)
	}

	// Update name only keeps password.
	if _, err := store.Update(view.ID, "Again", "", true, false); err != nil {
		t.Fatal(err)
	}
	plain3, err := store.DecryptAdminPassword(pk)
	if err != nil {
		t.Fatal(err)
	}
	if plain3 != "new-secret" {
		t.Fatalf("password changed unexpectedly: %q", plain3)
	}

	if err := store.Delete(view.ID); err != nil {
		t.Fatal(err)
	}
	list, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}
}

func TestNormalizePublicKey(t *testing.T) {
	pk := "AABBCCDDEEFF00112233445566778899AABBCCDDEEFF00112233445566778899"
	got, err := NormalizePublicKey("0x" + pk)
	if err != nil {
		t.Fatal(err)
	}
	if got != "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899" {
		t.Fatalf("got %q", got)
	}
	if _, err := NormalizePublicKey("short"); err != ErrInvalidKey {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
}

func TestWrongKeyCannotDecrypt(t *testing.T) {
	dir := t.TempDir()
	k1, _ := DeriveKey("vault-key-one", "")
	store, err := Open(dir, k1)
	if err != nil {
		t.Fatal(err)
	}
	pk := "11223344556677889900aabbccddeeff11223344556677889900aabbccddeeff"
	if _, err := store.Add(pk, "", "secret"); err != nil {
		t.Fatal(err)
	}

	k2, _ := DeriveKey("vault-key-two", "")
	store2 := &Store{path: filepath.Join(dir, "data", fileName), key: k2}
	if _, err := store2.DecryptAdminPassword(pk); err != ErrCorrupt {
		t.Fatalf("expected ErrCorrupt with wrong key, got %v", err)
	}
}

func TestVaultFileMode(t *testing.T) {
	dir := t.TempDir()
	key, _ := DeriveKey("", "mode-check-key")
	store, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("vault file too permissive: %v", info.Mode())
	}
}
