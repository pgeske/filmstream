package indexer

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/config"
)

type Source struct {
	Protocol   string `json:"protocol,omitempty"`
	MagnetURI  string `json:"magnet_uri,omitempty"`
	TorrentURL string `json:"torrent_url,omitempty"`
	NZBURL     string `json:"nzb_url,omitempty"`
}

type Indexer interface {
	Name() string
	Search(context.Context, catalog.SearchRequest) ([]catalog.Candidate, error)
	Resolve(context.Context, catalog.Candidate) (Source, error)
}

type Registry struct {
	mu       sync.RWMutex
	indexers map[string]Indexer
	ordered  []Indexer
	private  map[string]bool
}

// SearchPolicy bounds a search across every configured indexer.
type SearchPolicy struct {
	// Protocol keeps only candidates of this protocol; empty keeps every protocol.
	Protocol string
	// Acceptable reports whether the candidates gathered so far could be played.
	// Once it first holds, slower indexers get Settle longer to answer so a slow
	// private tracker can still beat a fast public one without holding the click
	// hostage. Nil (or a zero Settle) waits for every indexer.
	Acceptable func([]catalog.Candidate) bool
	Settle     time.Duration
	// Deadline caps the whole search. Indexers that have not answered by then
	// are abandoned and reported as timed out. Zero means no cap beyond ctx.
	Deadline time.Duration
}

func NewRegistry(configs []config.Indexer) (*Registry, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	registry := &Registry{indexers: make(map[string]Indexer), private: make(map[string]bool)}
	for _, cfg := range configs {
		var implementation Indexer
		var err error
		switch cfg.Type {
		case "open_media":
			implementation = NewOpenMedia(cfg.Name, cfg.Endpoint)
		case "internet_archive":
			implementation = NewInternetArchive(cfg.Name, cfg.Endpoint, client)
		case "torznab":
			implementation, err = NewTorznab(cfg.Name, cfg.Endpoint, cfg.APIKey, client)
			if err != nil {
				return nil, fmt.Errorf("configure indexer %q: %w", cfg.Name, err)
			}
		default:
			return nil, fmt.Errorf("indexer %q has unsupported type %q", cfg.Name, cfg.Type)
		}
		if _, exists := registry.indexers[cfg.Name]; exists {
			return nil, fmt.Errorf("duplicate indexer name %q", cfg.Name)
		}
		registry.indexers[cfg.Name] = implementation
		registry.ordered = append(registry.ordered, implementation)
		registry.private[cfg.Name] = cfg.Private
	}
	return registry, nil
}

func (r *Registry) Replace(configs []config.Indexer) error {
	replacement, err := NewRegistry(configs)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.indexers = replacement.indexers
	r.ordered = replacement.ordered
	r.private = replacement.private
	r.mu.Unlock()
	return nil
}

// Private reports whether the named indexer is configured as a private tracker.
func (r *Registry) Private(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.private[name]
}

// Search queries every configured indexer concurrently and returns when all of
// them answered, when policy.Settle elapsed after the gathered candidates first
// became acceptable, or at policy.Deadline, whichever comes first. Candidates
// carry their indexer name, private-tracker flag, and (for magnets) info hash.
// If some indexers failed or timed out, the partial candidates are returned
// with an error so callers can tell an incomplete search from an empty one.
func (r *Registry) Search(
	ctx context.Context,
	request catalog.SearchRequest,
	policy SearchPolicy,
) ([]catalog.Candidate, error) {
	r.mu.RLock()
	ordered := append([]Indexer(nil), r.ordered...)
	private := make(map[string]bool, len(r.private))
	for name, value := range r.private {
		private[name] = value
	}
	r.mu.RUnlock()
	if len(ordered) == 0 {
		return nil, errors.New("no indexers are configured")
	}

	searchContext, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		name       string
		candidates []catalog.Candidate
		err        error
	}
	results := make(chan result, len(ordered))
	for _, configured := range ordered {
		go func(configured Indexer) {
			candidates, err := configured.Search(searchContext, request)
			name := configured.Name()
			matches := make([]catalog.Candidate, 0, len(candidates))
			for _, candidate := range candidates {
				if policy.Protocol != "" && candidate.Protocol != policy.Protocol {
					continue
				}
				candidate.Indexer = name
				candidate.Private = private[name]
				if candidate.InfoHash == "" {
					candidate.InfoHash = magnetInfoHash(candidate.MagnetURI)
				}
				matches = append(matches, candidate)
			}
			results <- result{name: name, candidates: matches, err: err}
		}(configured)
	}

	var deadline <-chan time.Time
	if policy.Deadline > 0 {
		timer := time.NewTimer(policy.Deadline)
		defer timer.Stop()
		deadline = timer.C
	}
	var settle <-chan time.Time
	answered := make(map[string]bool, len(ordered))
	var candidates []catalog.Candidate
	var failures []string
	successes := 0
collect:
	for len(answered) < len(ordered) {
		select {
		case result := <-results:
			answered[result.name] = true
			if result.err != nil {
				failures = append(failures, result.err.Error())
				continue
			}
			successes++
			candidates = append(candidates, result.candidates...)
			if settle == nil && policy.Acceptable != nil && policy.Settle > 0 && policy.Acceptable(candidates) {
				timer := time.NewTimer(policy.Settle)
				defer timer.Stop()
				settle = timer.C
			}
		case <-settle:
			// Enough to play; stop waiting for slower indexers.
			break collect
		case <-deadline:
			for _, configured := range ordered {
				if !answered[configured.Name()] {
					failures = append(failures, fmt.Sprintf("%s timed out after %s", configured.Name(), policy.Deadline))
				}
			}
			break collect
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if len(failures) > 0 {
		if successes == 0 {
			return nil, fmt.Errorf("all indexers failed: %s", strings.Join(failures, "; "))
		}
		return candidates, fmt.Errorf("some indexers failed: %s", strings.Join(failures, "; "))
	}
	return candidates, nil
}

// magnetInfoHash extracts the normalized v1 info hash from a magnet link.
func magnetInfoHash(magnetURI string) string {
	if !strings.HasPrefix(magnetURI, "magnet:?") {
		return ""
	}
	values, err := url.ParseQuery(strings.TrimPrefix(magnetURI, "magnet:?"))
	if err != nil {
		return ""
	}
	for _, topic := range values["xt"] {
		if hash, ok := strings.CutPrefix(strings.ToLower(topic), "urn:btih:"); ok {
			if normalized := normalizeInfoHash(hash); normalized != "" {
				return normalized
			}
		}
	}
	return ""
}

// normalizeInfoHash returns a v1 info hash as 40 lowercase hex digits, accepting
// the hex or base32 encodings found in magnet links and indexer attributes.
func normalizeInfoHash(value string) string {
	value = strings.TrimSpace(value)
	switch len(value) {
	case 40:
		if decoded, err := hex.DecodeString(value); err == nil {
			return hex.EncodeToString(decoded)
		}
	case 32:
		if decoded, err := base32.StdEncoding.DecodeString(strings.ToUpper(value)); err == nil {
			return hex.EncodeToString(decoded)
		}
	}
	return ""
}

func (r *Registry) Resolve(ctx context.Context, candidate catalog.Candidate) (Source, error) {
	r.mu.RLock()
	configured, ok := r.indexers[candidate.Indexer]
	r.mu.RUnlock()
	if !ok {
		return Source{}, fmt.Errorf("indexer %q is not configured", candidate.Indexer)
	}
	return configured.Resolve(ctx, candidate)
}
