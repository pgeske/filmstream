package api

import (
	"context"
	"errors"
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
	key := showReleaseSearchKey(request)
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
		if err != nil && searchContext.Err() == nil {
			s.logger.Warn("prewarm TV release search", "media_id", request.MediaID, "error", err)
		}
	}()
}

func (s *Server) cachedShowReleases(
	ctx context.Context,
	request CreatePlaybackRequest,
) ([]catalog.RankedCandidate, bool) {
	key := showReleaseSearchKey(request)
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

// invalidateShowReleaseSearch prevents a retry from replaying the same stale
// prefetched ranking after a selected torrent proves unable to serve. The next
// playback request must ask the indexer for current candidates.
func (s *Server) invalidateShowReleaseSearch(request CreatePlaybackRequest) bool {
	key := showReleaseSearchKey(request)
	if key == "" {
		return false
	}
	s.releaseSearchMu.Lock()
	state := s.releaseSearches[key]
	if state == nil {
		s.releaseSearchMu.Unlock()
		return false
	}
	delete(s.releaseSearches, key)
	select {
	case <-state.ready:
	default:
		state.err = errors.New("release search invalidated after unavailable playback")
		close(state.ready)
	}
	s.releaseSearchMu.Unlock()
	s.logger.Info("invalidated prefetched TV release search after unavailable playback",
		"media_id", request.MediaID)
	return true
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

func showReleaseSearchKey(request CreatePlaybackRequest) string {
	seasonID := seasonPlaybackCacheMediaID(request)
	if seasonID == "" || strings.TrimSpace(request.Query) == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s", seasonID, strings.ToLower(strings.TrimSpace(request.Query)), strings.ToLower(request.Preferences.Resolution))
}
