package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseImportPermanentAndSTS(t *testing.T) {
	permanent, err := ParseImport([]byte(`{"CODEARTS_AK":"a","CODEARTS_SK":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(permanent) != 1 || permanent[0].UserID == "" || permanent[0].Refreshable() {
		t.Fatalf("bad permanent import: %#v", permanent)
	}
	expires := time.Now().Add(time.Hour).Format(time.RFC3339)
	raw := `{"user_id":"temp","credentials":{"access_key_id":"ak","secret_access_key":"sk","security_token":"st","expiration":"` + expires + `"},"refresh_token":"partial"}`
	accounts, err := ParseImport([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].Token() != "st" || accounts[0].Refreshable() {
		t.Fatal("incomplete renewal material was advertised as refreshable")
	}
	for _, bad := range []string{`{"ak":"a"}`, `{"ak":"a","sk":"s","security_token":"st"}`, `{"ak":"a","sk":"s","security_token":"st","expiration":"2000-01-01T00:00:00Z"}`, `[{"ak":"a","sk":"s"},{"ak":"a","sk":"s"}]`} {
		if _, err := ParseImport([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid import %s", bad)
		}
	}
}

func TestCredentialPersistenceRejectsOverwriteAndTraversal(t *testing.T) {
	dir := t.TempDir()
	a := New(`../../outside\file:stream`, "n", "", "", "ak", "sk", "", "", "")
	if strings.ContainsAny(a.FileName(), `/\:`) {
		t.Fatal("unsafe filename")
	}
	if err := SaveNew(dir, a); err != nil {
		t.Fatal(err)
	}
	if err := SaveNew(dir, a); !os.IsExist(err) {
		t.Fatalf("overwrite error=%v", err)
	}
	loaded, err := LoadDir(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("reload %d: %v", len(loaded), err)
	}
	_, ak, sk := loaded[0].Credentials()
	if ak != "ak" || sk != "sk" {
		t.Fatal("permanent credentials lost")
	}
	if err := a.SetPathAndSave(filepath.Join(dir, "wrong-name.json")); err == nil {
		t.Fatal("accepted unexpected credential path")
	}
	if err := a.RemoveFile(); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateCredentials("st", "ak2", "sk2", time.Now().Add(time.Hour).Format(time.RFC3339), "rt"); err == nil {
		t.Fatal("removed credential resurrected")
	}
	if _, err := os.Stat(filepath.Join(dir, a.FileName())); !os.IsNotExist(err) {
		t.Fatal("removed file was recreated")
	}
}

func TestImportDoesNotExposeSecretsInValidationError(t *testing.T) {
	_, err := ParseImport([]byte(`{"ak":"sensitive-key","sk":"sensitive-secret","security_token":"token"}`))
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("invalid diagnostic: %v", err)
	}
	a := New("u", "n", "", "", "ak", "sk", "", "", "")
	data, err := json.Marshal(a)
	if err != nil || !strings.Contains(string(data), `"access_key_id":"ak"`) {
		t.Fatal("stored format changed")
	}
}
