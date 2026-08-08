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
	if cfg.MaxCandidateGiB != 60 {
		t.Fatalf("max size = %d", cfg.MaxCandidateGiB)
	}
	if cfg.SeedRatioTarget != 1 {
		t.Fatalf("ratio = %f", cfg.SeedRatioTarget)
	}
	if cfg.PreferredResolution != "1080p" {
		t.Fatalf("resolution = %q", cfg.PreferredResolution)
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
	if cfg.ReadaheadMiB != 64 || cfg.Listen != "127.0.0.1:8943" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(cfg.Indexers) != 0 {
		t.Fatalf("indexers = %+v", cfg.Indexers)
	}
}
