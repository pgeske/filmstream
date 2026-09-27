package api

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/metadata"
)

const (
	// releaseSearchDeadline caps one release search across every indexer and
	// title fallback; releaseSearchSettle is how long it keeps waiting for
	// slower indexers once a playable release was found, so a slow private
	// tracker can still beat a fast public one without holding up the click.
	releaseSearchDeadline = 12 * time.Second
	releaseSearchSettle   = 4 * time.Second
	// releaseMetadataWait bounds the (cached) TMDB lookups that refine a search.
	releaseMetadataWait = 3 * time.Second
	// releaseSearchTimeout bounds a background prefetch: metadata plus search.
	releaseSearchTimeout     = releaseMetadataWait + releaseSearchDeadline + time.Second
	releaseSearchTTL         = 10 * time.Minute
	releaseSearchNegativeTTL = 15 * time.Second
)

type releaseSearchState struct {
	ready     chan struct{}
	ranked    []catalog.RankedCandidate
	err       error
	expiresAt time.Time
}

func (s *Server) queueReleaseSearch(request CreatePlaybackRequest) {
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
		ranked, err := s.searchReleases(searchContext, request)
		cacheTTL := releaseSearchCacheTTL(ranked, err)

		s.releaseSearchMu.Lock()
		if s.releaseSearches[key] == state {
			state.ranked = ranked
			state.err = err
			state.expiresAt = time.Now().Add(cacheTTL)
			close(state.ready)
		}
		s.releaseSearchMu.Unlock()
		s.logger.Info("prefetched release search stages", "media_id", request.MediaID,
			"candidates", len(ranked), "cache_ttl", cacheTTL,
			"total_duration", time.Since(started), "error", err)
		if err != nil && searchContext.Err() == nil {
			s.logger.Warn("prewarm release search", "media_id", request.MediaID, "error", err)
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
	ranked := make([]catalog.RankedCandidate, len(state.ranked))
	copy(ranked, state.ranked)
	return ranked, true
}

func (s *Server) searchReleases(
	ctx context.Context,
	request CreatePlaybackRequest,
) ([]catalog.RankedCandidate, error) {
	return s.searchAndRank(ctx, s.releaseSearchRequest(ctx, request), request.OriginalTitle, catalog.ProtocolTorrent)
}

// releaseSearchRequest builds the torrent search for a playback request: the
// IMDb/TMDB IDs of the movie or series for indexers that search by ID, the
// runtime that ranking turns into a bitrate estimate, and whether an episode
// prefers a season pack. Metadata that is not known within
// releaseMetadataWait is left out rather than delaying the search.
func (s *Server) releaseSearchRequest(ctx context.Context, request CreatePlaybackRequest) catalog.SearchRequest {
	search := catalog.SearchRequest{
		Query: request.Query, Year: request.Year, MediaType: request.MediaType,
		SeasonNumber: request.SeasonNumber, EpisodeNumber: request.EpisodeNumber,
		Preferences: request.Preferences,
	}
	isShow := request.MediaType == string(metadata.MediaTypeShow)
	titleID := request.MediaID
	if isShow {
		titleID = request.SeriesID
	}
	search.TMDBID = tmdbNumericID(titleID)

	s.metadataMu.RLock()
	provider := s.metadataProvider
	s.metadataMu.RUnlock()
	lookupContext, cancel := context.WithTimeout(ctx, releaseMetadataWait)
	defer cancel()
	var lookups sync.WaitGroup
	if isShow {
		lookups.Add(1)
		go func() {
			defer lookups.Done()
			search.PreferSeasonPack = s.shouldPreferSeasonPack(lookupContext, request)
		}()
	}
	imdbID := ""
	if provider, ok := provider.(metadata.IMDbIDProvider); ok && titleID != "" {
		lookups.Add(1)
		go func() {
			defer lookups.Done()
			if id, err := provider.IMDbID(lookupContext, titleID); err == nil {
				imdbID = id
			}
		}()
	}
	runtime, seasonRuntime := 0, 0
	if provider, ok := provider.(metadata.ShowProvider); ok && isShow && request.SeriesID != "" && request.SeasonNumber > 0 {
		lookups.Add(1)
		go func() {
			defer lookups.Done()
			if season, err := provider.Season(lookupContext, request.SeriesID, request.SeasonNumber); err == nil {
				runtime, seasonRuntime = episodeRuntimes(season, request.EpisodeNumber)
			}
		}()
	}
	if provider, ok := provider.(metadata.MovieRuntimeProvider); ok && !isShow && request.MediaID != "" {
		lookups.Add(1)
		go func() {
			defer lookups.Done()
			if minutes, err := provider.MovieRuntime(lookupContext, request.MediaID); err == nil {
				runtime = minutes
			}
		}()
	}
	lookups.Wait()
	search.IMDBID = imdbID
	search.RuntimeMinutes, search.SeasonRuntimeMinutes = runtime, seasonRuntime
	return search
}

// episodeRuntimes returns the runtime of one episode and of the whole season in
// minutes; episodes without a runtime count as the season's average.
func episodeRuntimes(season metadata.Season, episodeNumber int) (int, int) {
	known, total, episode := 0, 0, 0
	for _, candidate := range season.Episodes {
		if candidate.Runtime > 0 {
			known++
			total += candidate.Runtime
		}
		if candidate.EpisodeNumber == episodeNumber {
			episode = candidate.Runtime
		}
	}
	if known == 0 {
		return 0, 0
	}
	average := total / known
	if episode == 0 {
		episode = average
	}
	return episode, total + average*(len(season.Episodes)-known)
}

// tmdbNumericID extracts the numeric TMDB ID from a "tmdb:<id>" movie or
// "tmdb-tv:<id>" series ID; zero when the ID has another form.
func tmdbNumericID(mediaID string) int {
	for _, prefix := range []string{"tmdb-tv:", "tmdb:"} {
		if value, found := strings.CutPrefix(strings.TrimSpace(mediaID), prefix); found {
			if id, err := strconv.Atoi(value); err == nil && id > 0 {
				return id
			}
			return 0
		}
	}
	return 0
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

func releaseSearchCacheTTL(ranked []catalog.RankedCandidate, err error) time.Duration {
	if err != nil || len(ranked) == 0 {
		return releaseSearchNegativeTTL
	}
	return releaseSearchTTL
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
		expiresAt: time.Now().Add(releaseSearchCacheTTL(ranked, nil)),
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
	return fmt.Sprintf("%s/%s/%s/%s/%d/%d/%d/%s/%s/%s/%d/%t",
		strings.ToLower(strings.TrimSpace(request.MediaType)), mediaID, query,
		strings.ToLower(strings.TrimSpace(request.OriginalTitle)), request.Year,
		request.SeasonNumber, request.EpisodeNumber,
		strings.ToLower(preferences.Resolution), normalizedReleaseSearchValues(preferences.Codecs),
		normalizedReleaseSearchValues(preferences.Languages), preferences.MaxSizeBytes,
		preferences.StreamingOptimized)
}

func normalizedReleaseSearchValues(values []string) string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			normalized = append(normalized, value)
		}
	}
	slices.Sort(normalized)
	return strings.Join(normalized, ",")
}
