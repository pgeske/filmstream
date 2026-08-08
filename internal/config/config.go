package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	defaultListen          = "127.0.0.1:8943"
	defaultMaxCandidateGiB = 60
	defaultReadaheadMiB    = 32
)

type Indexer struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

type Config struct {
	Listen              string    `json:"listen"`
	DataDir             string    `json:"data_dir"`
	MaxCandidateGiB     int64     `json:"max_candidate_gib"`
	ReadaheadMiB        int64     `json:"readahead_mib"`
	MetadataTimeoutSecs int       `json:"metadata_timeout_seconds"`
	SeedRatioTarget     float64   `json:"seed_ratio_target"`
	PreferredResolution string    `json:"preferred_resolution"`
	PreferredLanguages  []string  `json:"preferred_languages"`
	Player              string    `json:"player"`
	Indexers            []Indexer `json:"indexers"`
}

func Defaults() Config {
	return Config{
		Listen:              defaultListen,
		DataDir:             defaultDataDir(),
		MaxCandidateGiB:     defaultMaxCandidateGiB,
		ReadaheadMiB:        defaultReadaheadMiB,
		MetadataTimeoutSecs: 120,
		SeedRatioTarget:     1,
		PreferredResolution: "1080p",
		PreferredLanguages:  []string{"en", "english"},
		Player:              "mpv",
		Indexers: []Indexer{
			{
				Name:     "open-media",
				Type:     "open_media",
				Endpoint: "https://webtorrent.io/torrents",
			},
			{
				Name:     "internet-archive",
				Type:     "internet_archive",
				Endpoint: "https://archive.org",
			},
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		path = defaultConfigPath()
	}

	contents, err := os.ReadFile(expandHome(path))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(contents, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.DataDir = expandHome(cfg.DataDir)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address cannot be empty")
	}
	if c.DataDir == "" {
		return errors.New("data directory cannot be empty")
	}
	if c.MaxCandidateGiB <= 0 {
		return errors.New("max_candidate_gib must be positive")
	}
	if c.ReadaheadMiB <= 0 {
		return errors.New("readahead_mib must be positive")
	}
	if c.MetadataTimeoutSecs <= 0 {
		return errors.New("metadata_timeout_seconds must be positive")
	}
	if c.SeedRatioTarget < 0 {
		return errors.New("seed_ratio_target cannot be negative")
	}
	for i, indexer := range c.Indexers {
		if indexer.Name == "" || indexer.Type == "" || indexer.Endpoint == "" {
			return fmt.Errorf("indexers[%d] must have name, type, and endpoint", i)
		}
	}
	return nil
}

func (c Config) MaxCandidateBytes() int64 {
	return c.MaxCandidateGiB << 30
}

func (c Config) ReadaheadBytes() int64 {
	return c.ReadaheadMiB << 20
}

func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "filmstream", "config.json")
	}
	return filepath.Join("~", ".config", "filmstream", "config.json")
}

func defaultDataDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "filmstream")
	}
	return filepath.Join("~", ".cache", "filmstream")
}

func expandHome(path string) string {
	if path == "~" || len(path) > 2 && path[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
