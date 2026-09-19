package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func TestUserAgentStorePersistsCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metroserver.db")
	store, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.recordUserAgent("okhttp/4.12.0"); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUserAgent("okhttp/4.12.0"); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUserAgent(""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	counts, err := store.userAgentCounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts["okhttp/4.12.0"] != 2 {
		t.Fatalf("persisted counts = %#v", counts)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions = %v, want 0600", info.Mode().Perm())
	}
}

func TestUserAgentStoreBoundsKeysAndLength(t *testing.T) {
	store, err := openDatabase(filepath.Join(t.TempDir(), "metroserver.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	longUserAgent := strings.Repeat("a", MaxUserAgentLength+20)
	if err := store.recordUserAgent(longUserAgent); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(userAgentsBucket).SetSequence(MaxTrackedUserAgents)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUserAgent(longUserAgent); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUserAgent("new-agent"); err != nil {
		t.Fatal(err)
	}

	counts, err := store.userAgentCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[strings.Repeat("a", MaxUserAgentLength)] != 2 {
		t.Fatalf("bounded User-Agent count = %#v", counts)
	}
	if _, exists := counts["new-agent"]; exists {
		t.Fatal("database accepted a new User-Agent after reaching its key limit")
	}
}

func TestUserAgentsHandlerRequiresToken(t *testing.T) {
	store, err := openDatabase(filepath.Join(t.TempDir(), "metroserver.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.recordUserAgent("ktor-client/3"); err != nil {
		t.Fatal(err)
	}
	server := testServer()
	server.database = store
	handler := server.userAgentsHandler("secret")

	unauthorized := httptest.NewRecorder()
	handler(unauthorized, httptest.NewRequest(http.MethodGet, "/uas", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/uas", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized status = %d", response.Code)
	}
	var counts map[string]uint64
	if err := json.NewDecoder(response.Body).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	if counts["ktor-client/3"] != 1 {
		t.Fatalf("response = %#v", counts)
	}
}
