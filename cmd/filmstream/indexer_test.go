package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pgeske/filmstream/internal/config"
)

func TestIndexerAddTestAndRemove(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q"/></searching></caps>`)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	initial := config.Defaults()
	initial.Listen = "127.0.0.1:1"
	if err := config.Save(path, initial); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FILMSTREAM_INDEXER_API_KEY", "secret")
	if err := runIndexerAdd([]string{"--config", path, "--name", "test", server.URL + "/1/api"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := findIndexer(cfg.Indexers, "test")
	if !ok || registered.Type != "torznab" || registered.APIKey != "secret" {
		t.Fatalf("registered indexer = %+v, found = %v", registered, ok)
	}
	if err := runIndexerTest([]string{"--config", path, "test"}); err != nil {
		t.Fatal(err)
	}
	if err := runIndexerRemove([]string{"--config", path, "test"}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findIndexer(cfg.Indexers, "test"); ok {
		t.Fatal("indexer still exists after removal")
	}
}
