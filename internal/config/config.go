package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	defaultListen            = "127.0.0.1:8943"
	defaultMaxCandidateGiB   = 60
	defaultReadaheadMiB      = 32
	defaultCacheLimitGiB     = 20
	defaultMaxSeedSessions   = 20
	defaultSeedMaxHours      = 24
	defaultIdleGraceSeconds  = 120
	defaultHLSStartupSeconds = 90
	defaultHLSBufferSeconds  = 24
	defaultHLSReadRate       = 1.25
	defaultHLSSegmentSeconds = 4
)

type Indexer struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key,omitempty"`
}

type Resolver struct {
	Provider       string `json:"provider,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	Model          string `json:"model,omitempty"`
	APIKeyEnv      string `json:"api_key_env,omitempty"`
	APIKeyFile     string `json:"api_key_file,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type Metadata struct {
	Provider       string `json:"provider,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	Language       string `json:"language,omitempty"`
	APIKeyEnv      string `json:"api_key_env,omitempty"`
	APIKeyFile     string `json:"api_key_file,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type Usenet struct {
	Provider              string `json:"provider,omitempty"`
	BaseURL               string `json:"base_url,omitempty"`
	APIKeyEnv             string `json:"api_key_env,omitempty"`
	APIKeyFile            string `json:"api_key_file,omitempty"`
	WebDAVUser            string `json:"webdav_user,omitempty"`
	WebDAVPasswordEnv     string `json:"webdav_password_env,omitempty"`
	WebDAVPasswordFile    string `json:"webdav_password_file,omitempty"`
	Category              string `json:"category,omitempty"`
	StartupTimeoutSeconds int    `json:"startup_timeout_seconds,omitempty"`
}

type Config struct {
	Listen              string    `json:"listen"`
	DataDir             string    `json:"data_dir"`
	StateDir            string    `json:"state_dir"`
	HLSDir              string    `json:"hls_dir"`
	FFmpegPath          string    `json:"ffmpeg_path"`
	FFprobePath         string    `json:"ffprobe_path"`
	HLSStartupSeconds   int       `json:"hls_startup_seconds"`
	HLSBufferSeconds    int       `json:"hls_startup_buffer_seconds"`
	HLSReadRate         float64   `json:"hls_read_rate"`
	HLSSegmentSeconds   int       `json:"hls_segment_seconds"`
	MaxCandidateGiB     int64     `json:"max_candidate_gib"`
	ReadaheadMiB        int64     `json:"readahead_mib"`
	MetadataTimeoutSecs int       `json:"metadata_timeout_seconds"`
	SeedRatioTarget     float64   `json:"seed_ratio_target"`
	CacheLimitGiB       int64     `json:"cache_limit_gib"`
	MaxSeedSessions     int       `json:"max_seed_sessions"`
	SeedMaxHours        int       `json:"seed_max_hours"`
	IdleGraceSeconds    int       `json:"idle_grace_seconds"`
	PreferredResolution string    `json:"preferred_resolution"`
	PreferredLanguages  []string  `json:"preferred_languages"`
	Player              string    `json:"player"`
	Resolver            Resolver  `json:"resolver,omitempty"`
	Metadata            Metadata  `json:"metadata,omitempty"`
	Usenet              Usenet    `json:"usenet,omitempty"`
	Indexers            []Indexer `json:"indexers"`
}

func Defaults() Config {
	return Config{
		Listen:              defaultListen,
		DataDir:             defaultDataDir(),
		StateDir:            defaultStateDir(),
		HLSDir:              filepath.Join(defaultDataDir(), "hls"),
		FFmpegPath:          "ffmpeg",
		FFprobePath:         "ffprobe",
		HLSStartupSeconds:   defaultHLSStartupSeconds,
		HLSBufferSeconds:    defaultHLSBufferSeconds,
		HLSReadRate:         defaultHLSReadRate,
		HLSSegmentSeconds:   defaultHLSSegmentSeconds,
		MaxCandidateGiB:     defaultMaxCandidateGiB,
		ReadaheadMiB:        defaultReadaheadMiB,
		MetadataTimeoutSecs: 120,
		SeedRatioTarget:     1,
		CacheLimitGiB:       defaultCacheLimitGiB,
		MaxSeedSessions:     defaultMaxSeedSessions,
		SeedMaxHours:        defaultSeedMaxHours,
		IdleGraceSeconds:    defaultIdleGraceSeconds,
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
	path = Path(path)

	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg.DataDir = expandHome(cfg.DataDir)
		cfg.StateDir = expandHome(cfg.StateDir)
		return cfg, cfg.Validate()
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(contents, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.DataDir = expandHome(cfg.DataDir)
	cfg.StateDir = expandHome(cfg.StateDir)
	cfg.HLSDir = expandHome(cfg.HLSDir)
	cfg.FFmpegPath = expandHome(cfg.FFmpegPath)
	cfg.FFprobePath = expandHome(cfg.FFprobePath)
	cfg.Resolver.APIKeyFile = expandHome(cfg.Resolver.APIKeyFile)
	cfg.Metadata.APIKeyFile = expandHome(cfg.Metadata.APIKeyFile)
	cfg.Usenet.APIKeyFile = expandHome(cfg.Usenet.APIKeyFile)
	cfg.Usenet.WebDAVPasswordFile = expandHome(cfg.Usenet.WebDAVPasswordFile)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	path = Path(path)
	if err := cfg.Validate(); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	contents = append(contents, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func Path(path string) string {
	if path == "" {
		path = defaultConfigPath()
	}
	return expandHome(path)
}

func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address cannot be empty")
	}
	if c.DataDir == "" {
		return errors.New("data directory cannot be empty")
	}
	if c.StateDir == "" {
		return errors.New("state directory cannot be empty")
	}
	if c.HLSDir == "" || c.FFmpegPath == "" || c.FFprobePath == "" {
		return errors.New("hls_dir, ffmpeg_path, and ffprobe_path cannot be empty")
	}
	if c.HLSStartupSeconds <= 0 || c.HLSBufferSeconds <= 0 || c.HLSSegmentSeconds <= 0 {
		return errors.New("HLS timeout, startup buffer, and segment duration must be positive")
	}
	if c.HLSReadRate < 1 || c.HLSReadRate > 4 {
		return errors.New("hls_read_rate must be between 1 and 4")
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
	if c.CacheLimitGiB <= 0 {
		return errors.New("cache_limit_gib must be positive")
	}
	if c.MaxSeedSessions <= 0 {
		return errors.New("max_seed_sessions must be positive")
	}
	if c.SeedMaxHours <= 0 {
		return errors.New("seed_max_hours must be positive")
	}
	if c.IdleGraceSeconds < 0 {
		return errors.New("idle_grace_seconds cannot be negative")
	}
	if c.Resolver.Provider != "" {
		if c.Resolver.Provider != "openai-compatible" {
			return fmt.Errorf("unsupported resolver provider %q", c.Resolver.Provider)
		}
		if c.Resolver.BaseURL == "" || c.Resolver.Model == "" {
			return errors.New("resolver base_url and model are required")
		}
		if c.Resolver.TimeoutSeconds < 0 {
			return errors.New("resolver timeout_seconds cannot be negative")
		}
	}
	if c.Metadata.Provider != "" {
		if c.Metadata.Provider != "tmdb" {
			return fmt.Errorf("unsupported metadata provider %q", c.Metadata.Provider)
		}
		if c.Metadata.TimeoutSeconds < 0 {
			return errors.New("metadata timeout_seconds cannot be negative")
		}
	}
	if c.Usenet.Provider != "" {
		if c.Usenet.Provider != "infinidysk" {
			return fmt.Errorf("unsupported Usenet provider %q", c.Usenet.Provider)
		}
		if c.Usenet.BaseURL == "" || c.Usenet.WebDAVUser == "" {
			return errors.New("Usenet base_url and webdav_user are required")
		}
		if c.Usenet.Category == "" {
			return errors.New("Usenet category is required")
		}
		if c.Usenet.StartupTimeoutSeconds < 0 {
			return errors.New("Usenet startup_timeout_seconds cannot be negative")
		}
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

func (c Config) CacheLimitBytes() int64 {
	return c.CacheLimitGiB << 30
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

func defaultStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "filmstream")
	}
	return filepath.Join("~", ".local", "state", "filmstream")
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
