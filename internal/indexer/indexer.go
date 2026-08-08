package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/config"
)

type Source struct {
	MagnetURI  string `json:"magnet_uri,omitempty"`
	TorrentURL string `json:"torrent_url,omitempty"`
}

type Indexer interface {
	Name() string
	Search(context.Context, catalog.SearchRequest) ([]catalog.Candidate, error)
	Resolve(context.Context, catalog.Candidate) (Source, error)
}

type Registry struct {
	indexers map[string]Indexer
	ordered  []Indexer
}

func NewRegistry(configs []config.Indexer) (*Registry, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	registry := &Registry{indexers: make(map[string]Indexer)}
	for _, cfg := range configs {
		var implementation Indexer
		switch cfg.Type {
		case "open_media":
			implementation = NewOpenMedia(cfg.Name, cfg.Endpoint)
		case "internet_archive":
			implementation = NewInternetArchive(cfg.Name, cfg.Endpoint, client)
		case "http":
			implementation = NewHTTP(cfg.Name, cfg.Endpoint, cfg.Headers, client)
		default:
			return nil, fmt.Errorf("indexer %q has unsupported type %q", cfg.Name, cfg.Type)
		}
		if _, exists := registry.indexers[cfg.Name]; exists {
			return nil, fmt.Errorf("duplicate indexer name %q", cfg.Name)
		}
		registry.indexers[cfg.Name] = implementation
		registry.ordered = append(registry.ordered, implementation)
	}
	return registry, nil
}

func (r *Registry) Search(ctx context.Context, request catalog.SearchRequest) ([]catalog.Candidate, error) {
	if len(r.ordered) == 0 {
		return nil, errors.New("no indexers are configured")
	}

	type result struct {
		candidates []catalog.Candidate
		err        error
	}
	results := make(chan result, len(r.ordered))
	var group sync.WaitGroup
	for _, configured := range r.ordered {
		group.Add(1)
		go func(indexer Indexer) {
			defer group.Done()
			candidates, err := indexer.Search(ctx, request)
			for i := range candidates {
				candidates[i].Indexer = indexer.Name()
			}
			results <- result{candidates: candidates, err: err}
		}(configured)
	}
	group.Wait()
	close(results)

	var candidates []catalog.Candidate
	var failures []string
	for result := range results {
		candidates = append(candidates, result.candidates...)
		if result.err != nil {
			failures = append(failures, result.err.Error())
		}
	}
	if len(candidates) == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("all indexers failed: %s", strings.Join(failures, "; "))
	}
	return candidates, nil
}

func (r *Registry) Resolve(ctx context.Context, candidate catalog.Candidate) (Source, error) {
	configured, ok := r.indexers[candidate.Indexer]
	if !ok {
		return Source{}, fmt.Errorf("indexer %q is not configured", candidate.Indexer)
	}
	return configured.Resolve(ctx, candidate)
}
