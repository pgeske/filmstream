package metadata

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pgeske/filmstream/internal/config"
)

const defaultTMDBBaseURL = "https://api.themoviedb.org/3"

func FromConfig(cfg config.Metadata) (Provider, error) {
	if cfg.Provider == "" {
		return nil, nil
	}
	apiKey := ""
	if cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}
	if apiKey == "" && cfg.APIKeyFile != "" {
		contents, err := os.ReadFile(cfg.APIKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read metadata API key file: %w", err)
		}
		apiKey = strings.TrimSpace(string(contents))
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	switch cfg.Provider {
	case "tmdb":
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = defaultTMDBBaseURL
		}
		return NewTMDB(baseURL, apiKey, cfg.Language, &http.Client{Timeout: timeout})
	default:
		return nil, fmt.Errorf("unsupported metadata provider %q", cfg.Provider)
	}
}
