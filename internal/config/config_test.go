package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsMatchLocalMVP(t *testing.T) {
	cfg := Defaults()
	if cfg.Listen != "127.0.0.1:8943" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.MaxCandidateGiB != 20 {
		t.Fatalf("max size = %d", cfg.MaxCandidateGiB)
	}
	if cfg.SeedRatioTarget != 1 {
		t.Fatalf("ratio = %f", cfg.SeedRatioTarget)
	}
	if cfg.CacheLimitGiB != 20 || cfg.MaxSeedSessions != 20 || cfg.SeedMaxHours != 24 || cfg.IdleGraceSeconds != 120 {
		t.Fatalf("smart streaming defaults = cache %d GiB, sessions %d, seed %d hours, grace %d seconds",
			cfg.CacheLimitGiB, cfg.MaxSeedSessions, cfg.SeedMaxHours, cfg.IdleGraceSeconds)
	}
	if cfg.PreferredResolution != "1080p" {
		t.Fatalf("resolution = %q", cfg.PreferredResolution)
	}
}

func TestMissingConfigExpandsDefaultStateDirectory(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.StateDir) {
		t.Fatalf("state directory = %q", cfg.StateDir)
	}
}

func TestSaveUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg := Defaults()
	cfg.Indexers = append(cfg.Indexers, Indexer{
		Name: "private", Type: "torznab", Endpoint: "https://example.test/api", APIKey: "secret",
	})
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o", got)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Indexers[len(loaded.Indexers)-1].APIKey; got != "secret" {
		t.Fatalf("API key = %q", got)
	}
}

func TestLoadOverlaysDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"readahead_mib":64,"indexers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadaheadMiB != 64 || cfg.Listen != "127.0.0.1:8943" || cfg.StateDir == "" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(cfg.Indexers) != 0 {
		t.Fatalf("indexers = %+v", cfg.Indexers)
	}
}
