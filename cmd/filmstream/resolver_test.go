package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pgeske/filmstream/internal/config"
)

func TestResolverConfigureTestAndDisable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"candidates\":[{\"title\":\"The Movie\",\"year\":2001,\"confidence\":0.9}]}"}}]}`)
	}))
	defer server.Close()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	keyPath := filepath.Join(directory, "key")
	if err := os.WriteFile(keyPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Listen = "127.0.0.1:1"
	cfg.DataDir = filepath.Join(directory, "data")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	if err := runResolverConfigure([]string{
		"--config", configPath,
		"--base-url", server.URL + "/v1",
		"--model", "small",
		"--api-key-file", keyPath,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Resolver.Provider != "openai-compatible" || loaded.Resolver.Model != "small" {
		t.Fatalf("resolver config = %+v", loaded.Resolver)
	}
	if err := runResolverTest([]string{"--config", configPath, "rough movie"}); err != nil {
		t.Fatal(err)
	}
	if err := runResolverDisable([]string{"--config", configPath}); err != nil {
		t.Fatal(err)
	}
	loaded, err = config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Resolver.Provider != "" {
		t.Fatalf("resolver still configured: %+v", loaded.Resolver)
	}
}
