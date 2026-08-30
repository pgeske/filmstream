package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/metadata"
)

const (
	releaseSearchTimeout = 45 * time.Second
	releaseSearchTTL     = 10 * time.Minute
)

type releaseSearchState struct {
	ready     chan struct{}
	ranked    []catalog.RankedCandidate
	err       error
	expiresAt time.Time
}

func (s *Server) queueShowReleaseSearch(request CreatePlaybackRequest) {
	key := releaseSearchKey(request)
	if key == "" {
		return
	}
	s.prewarmMu.Lock()
	ctx := s.prewarmContext
	s.prewarmMu.Unlock()
	if ctx == nil {
		return
	}

	now := time.Now()
	s.releaseSearchMu.Lock()
	if s.releaseSearches == nil {
		s.releaseSearches = make(map[string]*releaseSearchState)
	}
	if existing := s.releaseSearches[key]; existing != nil &&
		(existing.expiresAt.IsZero() || now.Before(existing.expiresAt)) {
		s.releaseSearchMu.Unlock()
		return
	}
	state := &releaseSearchState{ready: make(chan struct{})}
	s.releaseSearches[key] = state
	s.releaseSearchMu.Unlock()

	go func() {
		started := time.Now()
		searchContext, cancel := context.WithTimeout(ctx, releaseSearchTimeout)
		defer cancel()
		ranked, err := s.searchShowReleases(searchContext, request)

		s.releaseSearchMu.Lock()
		if s.releaseSearches[key] == state {
			state.ranked = ranked
			state.err = err
			state.expiresAt = time.Now().Add(releaseSearchTTL)
			close(state.ready)
		}
		s.releaseSearchMu.Unlock()
		s.logger.Info("prefetched release search stages", "media_id", request.MediaID,
			"candidates", len(ranked), "cache_ttl", releaseSearchTTL,
			"total_duration", time.Since(started), "error", err)
		if err != nil && searchContext.Err() == nil {
			s.logger.Warn("prewarm TV release search", "media_id", request.MediaID, "error", err)
		}
	}()
}

func (s *Server) cachedReleaseSearch(
	ctx context.Context,
	request CreatePlaybackRequest,
) ([]catalog.RankedCandidate, bool) {
	key := releaseSearchKey(request)
	if key == "" {
		return nil, false
	}
	s.releaseSearchMu.Lock()
	state := s.releaseSearches[key]
	if state == nil || (!state.expiresAt.IsZero() && time.Now().After(state.expiresAt)) {
		delete(s.releaseSearches, key)
		s.releaseSearchMu.Unlock()
		return nil, false
	}
	ready := state.ready
	s.releaseSearchMu.Unlock()

	select {
	case <-ctx.Done():
		return nil, false
	case <-ready:
	}

	s.releaseSearchMu.Lock()
	defer s.releaseSearchMu.Unlock()
	if s.releaseSearches[key] != state || state.err != nil || time.Now().After(state.expiresAt) {
		return nil, false
	}
	return append([]catalog.RankedCandidate(nil), state.ranked...), true
}

func (s *Server) searchShowReleases(
	ctx context.Context,
	request CreatePlaybackRequest,
) ([]catalog.RankedCandidate, error) {
	return s.searchAndRank(ctx, catalog.SearchRequest{
		Query: request.Query, Year: request.Year, MediaType: request.MediaType,
		SeasonNumber: request.SeasonNumber, EpisodeNumber: request.EpisodeNumber,
		PreferSeasonPack: s.shouldPreferSeasonPack(ctx, request),
		Preferences:      request.Preferences,
	}, request.OriginalTitle, catalog.ProtocolTorrent)
}

func (s *Server) shouldPreferSeasonPack(ctx context.Context, request CreatePlaybackRequest) bool {
	if request.SeriesID == "" || request.SeasonNumber <= 0 {
		return true
	}
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.ShowProvider)
	s.metadataMu.RUnlock()
	if !ok {
		return true
	}
	show, err := provider.Show(ctx, request.SeriesID)
	if err != nil || show.NumberOfSeasons <= 0 || request.SeasonNumber < show.NumberOfSeasons {
		return true
	}
	season, err := provider.Season(ctx, request.SeriesID, request.SeasonNumber)
	if err != nil {
		return true
	}
	for _, summary := range show.Seasons {
		if summary.Number == request.SeasonNumber && summary.EpisodeCount > len(season.Episodes) {
			return false
		}
	}
	return true
}

func (s *Server) cacheReleaseSearch(request CreatePlaybackRequest, ranked []catalog.RankedCandidate) {
	key := releaseSearchKey(request)
	if key == "" {
		return
	}
	ready := make(chan struct{})
	close(ready)
	state := &releaseSearchState{
		ready: ready, ranked: append([]catalog.RankedCandidate(nil), ranked...),
		expiresAt: time.Now().Add(releaseSearchTTL),
	}
	s.releaseSearchMu.Lock()
	if s.releaseSearches == nil {
		s.releaseSearches = make(map[string]*releaseSearchState)
	}
	if existing := s.releaseSearches[key]; existing == nil || !existing.expiresAt.IsZero() {
		s.releaseSearches[key] = state
	}
	s.releaseSearchMu.Unlock()
}

// releaseSearchKey includes every request field that affects torrent ranking.
// Movie prewarms and show browsing can then retain the ranked candidates for a
// fast retry without sharing an episode-specific result with another request.
func releaseSearchKey(request CreatePlaybackRequest) string {
	query := strings.ToLower(strings.TrimSpace(request.Query))
	if query == "" {
		return ""
	}
	mediaID := strings.ToLower(strings.TrimSpace(request.MediaID))
	if mediaID == "" {
		mediaID = fmt.Sprintf("%s:%d", query, request.Year)
	}
	preferences := request.Preferences
	return fmt.Sprintf("%s/%s/%s/%d/%d/%s/%s/%s/%d/%t",
		strings.ToLower(strings.TrimSpace(request.MediaType)), mediaID, query,
		request.SeasonNumber, request.EpisodeNumber,
		strings.ToLower(preferences.Resolution), strings.Join(preferences.Codecs, ","),
		strings.Join(preferences.Languages, ","), preferences.MaxSizeBytes,
		preferences.StreamingOptimized)
}
