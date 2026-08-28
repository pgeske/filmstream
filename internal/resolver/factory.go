package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/config"
)

func FromConfig(cfg config.Resolver, dataDir string) (Resolver, error) {
	configured, err := UncachedFromConfig(cfg)
	if err != nil || configured == nil {
		return configured, err
	}
	namespace := strings.Join([]string{"movie-intent-v1", cfg.Provider, cfg.BaseURL, cfg.Model}, "/")
	return NewCached(configured, filepath.Join(dataDir, "resolver-cache.json"), namespace), nil
}

func UncachedFromConfig(cfg config.Resolver) (Resolver, error) {
	model, err := ModelFromConfig(cfg, "")
	if err != nil || model == nil {
		return nil, err
	}
	return NewSemantic(model, cfg.Provider, cfg.Model), nil
}

// ModelFromConfig builds a completion model with the resolver's existing endpoint,
// credentials, and timeout. modelOverride selects a different model on that same
// provider without introducing another credential path.
func ModelFromConfig(cfg config.Resolver, modelOverride string) (Model, error) {
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
			return nil, fmt.Errorf("read resolver API key file: %w", err)
		}
		apiKey = strings.TrimSpace(string(contents))
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	modelName := strings.TrimSpace(modelOverride)
	if modelName == "" {
		modelName = cfg.Model
	}

	switch cfg.Provider {
	case "openai-compatible":
		return NewOpenAICompatible(cfg.BaseURL, modelName, apiKey, &http.Client{Timeout: timeout})
	default:
		return nil, fmt.Errorf("unsupported resolver provider %q", cfg.Provider)
	}
}

type Cached struct {
	inner     Resolver
	path      string
	namespace string

	mu      sync.Mutex
	entries map[string]Result
}

func NewCached(inner Resolver, path, namespace string) *Cached {
	return &Cached{inner: inner, path: path, namespace: namespace, entries: make(map[string]Result)}
}

func (c *Cached) Resolve(ctx context.Context, input string) (Result, error) {
	key := c.namespace + ":" + strings.ToLower(strings.Join(strings.Fields(input), " "))
	c.mu.Lock()
	if len(c.entries) == 0 {
		_ = c.load()
	}
	if result, ok := c.entries[key]; ok {
		result.Input = strings.TrimSpace(input)
		result.Cached = true
		c.mu.Unlock()
		return result, nil
	}
	c.mu.Unlock()

	result, err := c.inner.Resolve(ctx, input)
	if err != nil {
		return Result{}, err
	}
	c.mu.Lock()
	c.entries[key] = result
	_ = c.save()
	c.mu.Unlock()
	return result, nil
}

func (c *Cached) load() error {
	contents, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(contents, &c.entries)
}

func (c *Cached) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(c.path), ".resolver-cache-*.json")
	if err != nil {
		return err
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
	return os.Rename(temporaryPath, c.path)
}
