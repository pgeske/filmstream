package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/config"
	"github.com/pgeske/filmstream/internal/history"
	"github.com/pgeske/filmstream/internal/hls"
	"github.com/pgeske/filmstream/internal/indexer"
	"github.com/pgeske/filmstream/internal/metadata"
	"github.com/pgeske/filmstream/internal/playbackcache"
	"github.com/pgeske/filmstream/internal/recommendations"
	"github.com/pgeske/filmstream/internal/resolver"
	"github.com/pgeske/filmstream/internal/torrentstream"
	"github.com/pgeske/filmstream/internal/usenetstream"
)

type CreatePlaybackRequest struct {
	MediaID       string              `json:"media_id,omitempty"`
	MediaType     string              `json:"media_type,omitempty"`
	Query         string              `json:"query,omitempty"`
	OriginalTitle string              `json:"original_title,omitempty"`
	Year          int                 `json:"year,omitempty"`
	SeriesID      string              `json:"series_id,omitempty"`
	SeriesTitle   string              `json:"series_title,omitempty"`
	SeasonNumber  int                 `json:"season_number,omitempty"`
	EpisodeNumber int                 `json:"episode_number,omitempty"`
	EpisodeTitle  string              `json:"episode_title,omitempty"`
	StartSeconds  float64             `json:"start_seconds,omitempty"`
	Preferences   catalog.Preferences `json:"preferences,omitempty"`
	MagnetURI     string              `json:"magnet_uri,omitempty"`
	TorrentPath   string              `json:"torrent_path,omitempty"`
}

const (
	maxUsenetCandidates     = 30
	maxUsenetPreparation    = 90 * time.Second
	usenetFailureTTL        = 30 * time.Minute
	maxLiveSwarmCandidates  = 3
	minimumLivePeers        = 3
	strongLiveSwarmPeers    = 20
	progressiveSwarmWait    = 750 * time.Millisecond
	cachedSwarmWait         = 3 * time.Second
	liveSwarmWait           = 8 * time.Second
	swarmStatusPollInterval = 250 * time.Millisecond

	subtitlesVerifiedReason = "subtitles verified"
	subtitleFallbackReason  = "subtitle fallback verified"
)

type CreatePlaybackResponse struct {
	ID        string                   `json:"id"`
	Name      string                   `json:"name"`
	FileName  string                   `json:"file_name"`
	FileSize  int64                    `json:"file_size"`
	Source    string                   `json:"source"`
	StreamURL string                   `json:"stream_url"`
	Selected  *catalog.RankedCandidate `json:"selected,omitempty"`
}

type playbackSubtitleSelection struct {
	Mode     string `json:"mode,omitempty"`
	Index    int    `json:"index,omitempty"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Codec    string `json:"codec,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
}

type playbackSession struct {
	ID       string
	Name     string
	FileName string
	FileSize int64
	Source   string
}

type catalogSection struct {
	ID       string           `json:"id"`
	Title    string           `json:"title"`
	Subtitle string           `json:"subtitle"`
	Items    []metadata.Movie `json:"items"`
}

type HLSStreamManager interface {
	ProbeSubtitles(context.Context, string) ([]hls.SubtitleTrack, error)
	Start(context.Context, string, float64, []string, int) (hls.Stream, error)
	StartSubtitle(context.Context, string, int) error
	AssetPath(string, string) (string, error)
	Prepared(string, float64, []string, int, int) bool
	Park(context.Context, string, int) error
	Stop(string)
}

type torrentPlaybackEngine interface {
	Create(context.Context, torrentstream.Source) (*torrentstream.Session, error)
	Get(string) (*torrentstream.Session, bool)
	TorrentMetainfo(string) ([]byte, error)
	Drop(string) error
	Status(string) (torrentstream.Status, bool)
	ServeHTTP(http.ResponseWriter, *http.Request, string) error
}

type usenetPlaybackEngine interface {
	SetCleanupHandler(func(string, string))
	Create(context.Context, usenetstream.Source) (*usenetstream.Session, error)
	Get(string) (*usenetstream.Session, bool)
	NZB(string) ([]byte, error)
	Status(string) (usenetstream.Status, bool)
	ServeHTTP(http.ResponseWriter, *http.Request, string) error
	Drop(string) error
}

type playbackCacheKey struct {
	mediaID string
	title   string
	year    int
	source  string
}

type playbackSelectionTiming struct {
	progressiveWait time.Duration
	liveSwarmWait   time.Duration
	statusPoll      time.Duration
}

type Server struct {
	indexers           *indexer.Registry
	engine             torrentPlaybackEngine
	usenetEngine       usenetPlaybackEngine
	playbackSourceMode string
	defaults           catalog.Preferences
	logger             *slog.Logger
	reloadIndexers     func() error
	reloadResolver     func() error

	resolverMu        sync.RWMutex
	movieResolver     resolver.Resolver
	metadataMu        sync.RWMutex
	metadataProvider  metadata.Provider
	ratingsProvider   metadata.RatingsProvider
	historyStore      *history.Store
	playbackCache     *playbackcache.Store
	hlsMu             sync.RWMutex
	hlsManager        HLSStreamManager
	mu                sync.RWMutex
	selected          map[string]catalog.RankedCandidate
	playbackCacheKeys map[string]playbackCacheKey
	playbackLanguages map[string][]string
	playbackRequests  map[string]CreatePlaybackRequest
	playbackResponses map[string]CreatePlaybackResponse
	usenetFailures    map[string]time.Time

	recommendationService recommendations.Manager

	prewarmMu      sync.Mutex
	prewarmStates  map[string]*playbackPrewarmState
	prewarmSlots   chan struct{}
	prewarmBaseURL string
	prewarmClient  *http.Client
	prewarmContext context.Context

	releaseSearchMu sync.Mutex
	releaseSearches map[string]*releaseSearchState

	selectionTiming playbackSelectionTiming
}

func New(indexers *indexer.Registry, engine *torrentstream.Engine, defaults catalog.Preferences, logger *slog.Logger) *Server {
	server := &Server{
		indexers:           indexers,
		engine:             engine,
		playbackSourceMode: config.PlaybackSourceHybrid,
		defaults:           defaults,
		logger:             logger,
		selected:           make(map[string]catalog.RankedCandidate),
		playbackCacheKeys:  make(map[string]playbackCacheKey),
		playbackLanguages:  make(map[string][]string),
		playbackRequests:   make(map[string]CreatePlaybackRequest),
		playbackResponses:  make(map[string]CreatePlaybackResponse),
		usenetFailures:     make(map[string]time.Time),
		prewarmStates:      make(map[string]*playbackPrewarmState),
		prewarmSlots:       make(chan struct{}, 2),
		releaseSearches:    make(map[string]*releaseSearchState),
	}
	engine.SetCleanupHandler(func(id, _ string) {
		server.cleanupPlayback(id)
	})
	return server
}

func (s *Server) SetPlaybackSourceMode(mode string) {
	s.playbackSourceMode = mode
}

func (s *Server) SetUsenetEngine(engine *usenetstream.Engine) {
	s.usenetEngine = engine
	if engine != nil {
		engine.SetCleanupHandler(func(id, _ string) {
			s.cleanupPlayback(id)
		})
	}
}

func (s *Server) cleanupPlayback(id string) {
	s.mu.Lock()
	delete(s.selected, id)
	delete(s.playbackCacheKeys, id)
	delete(s.playbackLanguages, id)
	delete(s.playbackRequests, id)
	delete(s.playbackResponses, id)
	s.mu.Unlock()
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager != nil {
		manager.Stop(id)
	}
}

func (s *Server) SetIndexerReloader(reload func() error) {
	s.reloadIndexers = reload
}

func (s *Server) SetMovieResolver(movieResolver resolver.Resolver) {
	s.resolverMu.Lock()
	s.movieResolver = movieResolver
	s.resolverMu.Unlock()
}

func (s *Server) SetResolverReloader(reload func() error) {
	s.reloadResolver = reload
}

func (s *Server) SetMetadataProvider(provider metadata.Provider) {
	s.metadataMu.Lock()
	s.metadataProvider = provider
	s.metadataMu.Unlock()
}

func (s *Server) SetRatingsProvider(provider metadata.RatingsProvider) {
	s.metadataMu.Lock()
	s.ratingsProvider = provider
	s.metadataMu.Unlock()
}

func (s *Server) SetHistoryStore(store *history.Store) {
	s.historyStore = store
}

func (s *Server) SetRecommendationService(service recommendations.Manager) {
	s.recommendationService = service
}

func (s *Server) SetPlaybackCache(store *playbackcache.Store) {
	s.playbackCache = store
	failures, err := store.LoadUsenetFailures()
	if err != nil {
		s.logger.Warn("load Usenet failure cache", "error", err)
		return
	}
	s.mu.Lock()
	s.usenetFailures = failures
	s.mu.Unlock()
}

func (s *Server) SetHLSManager(manager HLSStreamManager) {
	s.hlsMu.Lock()
	s.hlsManager = manager
	s.hlsMu.Unlock()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/indexers/reload", s.reloadIndexerConfiguration)
	mux.HandleFunc("POST /v1/resolver/reload", s.reloadResolverConfiguration)
	mux.HandleFunc("POST /v1/resolve", s.resolveMovie)
	mux.HandleFunc("GET /v1/catalog/search", s.searchCatalog)
	mux.HandleFunc("GET /v1/catalog/discover", s.discoverCatalog)
	mux.HandleFunc("GET /v1/catalog/shows/{id}", s.showDetails)
	mux.HandleFunc("GET /v1/catalog/shows/{id}/seasons/{season}", s.showSeason)
	mux.HandleFunc("GET /v1/catalog/ratings", s.catalogRatings)
	mux.HandleFunc("GET /v1/watch-history", s.listWatchHistory)
	mux.HandleFunc("PUT /v1/watch-history", s.updateWatchProgress)
	mux.HandleFunc("DELETE /v1/watch-history/{id}", s.removeWatchHistory)
	mux.HandleFunc("GET /v1/recommendations", s.listRecommendations)
	mux.HandleFunc("PUT /v1/recommendations/prompt", s.updateRecommendationPrompt)
	mux.HandleFunc("POST /v1/recommendations/refresh", s.refreshRecommendations)
	mux.HandleFunc("POST /v1/playbacks", s.createPlayback)
	mux.HandleFunc("POST /v1/playbacks/prewarm", s.prewarmPlayback)
	mux.HandleFunc("GET /v1/playbacks/{id}", s.playbackStatus)
	mux.HandleFunc("GET /v1/playbacks/{id}/stream", s.streamPlayback)
	mux.HandleFunc("HEAD /v1/playbacks/{id}/stream", s.streamPlayback)
	mux.HandleFunc("POST /v1/playbacks/{id}/hls", s.startHLSPlayback)
	mux.HandleFunc("POST /v1/playbacks/{id}/hls/park", s.parkHLSPlayback)
	mux.HandleFunc("DELETE /v1/playbacks/{id}/hls", s.stopHLSPlayback)
	mux.HandleFunc("GET /v1/playbacks/{id}/hls/subtitles", s.listHLSSubtitles)
	mux.HandleFunc("POST /v1/playbacks/{id}/hls/subtitles/{index}", s.startHLSSubtitle)
	mux.HandleFunc("GET /v1/playbacks/{id}/hls/{asset}", s.serveHLSAsset)
	return mux
}

func (s *Server) resolveMovie(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request struct {
		Query string `json:"query"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		writeError(w, http.StatusBadRequest, "query cannot be empty")
		return
	}
	s.resolverMu.RLock()
	movieResolver := s.movieResolver
	s.resolverMu.RUnlock()
	if movieResolver == nil {
		writeJSON(w, http.StatusOK, resolver.Result{
			Input:      request.Query,
			Candidates: []resolver.Candidate{{Title: request.Query, Confidence: 1}},
		})
		return
	}
	result, err := movieResolver.Resolve(r.Context(), request.Query)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) searchCatalog(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("q"))
	}
	if query == "" {
		writeError(w, http.StatusBadRequest, "query cannot be empty")
		return
	}
	movies, err := s.searchMovies(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if movies == nil {
		movies = []metadata.Movie{}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []metadata.Movie `json:"items"`
	}{Items: movies})
}

func (s *Server) catalogRatings(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" {
		writeError(w, http.StatusBadRequest, "title cannot be empty")
		return
	}
	year := 0
	if value := strings.TrimSpace(r.URL.Query().Get("year")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1888 || parsed > 2100 {
			writeError(w, http.StatusBadRequest, "year must be a valid release year")
			return
		}
		year = parsed
	}
	mediaID := strings.TrimSpace(r.URL.Query().Get("media_id"))
	s.metadataMu.RLock()
	ratingsProvider := s.ratingsProvider
	movieMetadataProvider := s.metadataProvider
	s.metadataMu.RUnlock()
	if ratingsProvider == nil {
		writeError(w, http.StatusServiceUnavailable, "external media ratings are not configured")
		return
	}

	var resolvedRatings metadata.MovieRatings
	hasResolvedRatings := false
	if idProvider, ok := movieMetadataProvider.(metadata.IMDbIDProvider); ok && mediaID != "" {
		if imdbRatingsProvider, ok := ratingsProvider.(metadata.IMDbRatingsProvider); ok {
			imdbID, idErr := idProvider.IMDbID(r.Context(), mediaID)
			if idErr == nil {
				ratings, ratingsErr := imdbRatingsProvider.RatingsByIMDbID(r.Context(), imdbID)
				if ratingsErr == nil {
					resolvedRatings = ratings
					hasResolvedRatings = true
					if ratings.IMDb != nil && ratings.RottenTomatoes != nil {
						writeJSON(w, http.StatusOK, ratings)
						return
					}
				} else if s.logger != nil {
					s.logger.Warn("load ratings by IMDb ID", "media_id", mediaID, "error", ratingsErr)
				}
			} else if s.logger != nil {
				s.logger.Warn("resolve IMDb ID", "media_id", mediaID, "error", idErr)
			}
		}
	}

	titleRatings, err := ratingsProvider.Ratings(r.Context(), title, year)
	if err != nil {
		if hasResolvedRatings {
			writeJSON(w, http.StatusOK, resolvedRatings)
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mergeMovieRatings(resolvedRatings, titleRatings))
}

func (s *Server) discoverCatalog(w http.ResponseWriter, r *http.Request) {
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.DiscoveryProvider)
	s.metadataMu.RUnlock()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "catalog discovery is not configured")
		return
	}

	collections := []struct {
		id       metadata.Collection
		title    string
		subtitle string
	}{
		{id: metadata.CollectionPopular, title: "Popular Now", subtitle: "Movies and shows everyone is watching"},
		{id: metadata.CollectionTopRated, title: "Top Rated", subtitle: "Acclaimed stories worth discovering"},
	}
	sections := make([]catalogSection, 0, len(collections))
	var firstError error
	for _, collection := range collections {
		movies, err := provider.Discover(r.Context(), collection.id)
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			if s.logger != nil {
				s.logger.Warn("load discovery collection", "collection", collection.id, "error", err)
			}
			continue
		}
		if movies == nil {
			movies = []metadata.Movie{}
		}
		sections = append(sections, catalogSection{
			ID:       string(collection.id),
			Title:    collection.title,
			Subtitle: collection.subtitle,
			Items:    movies,
		})
	}
	if len(sections) == 0 && firstError != nil {
		writeError(w, http.StatusBadGateway, firstError.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Sections []catalogSection `json:"sections"`
	}{Sections: sections})
}

func (s *Server) showDetails(w http.ResponseWriter, r *http.Request) {
	showID := strings.TrimSpace(r.PathValue("id"))
	if showID == "" {
		writeError(w, http.StatusBadRequest, "show ID cannot be empty")
		return
	}
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.ShowProvider)
	s.metadataMu.RUnlock()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "show metadata is not configured")
		return
	}
	show, err := provider.Show(r.Context(), showID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Show    metadata.Movie           `json:"show"`
		Seasons []metadata.SeasonSummary `json:"seasons"`
	}{Show: show.Movie, Seasons: show.Seasons})
}

func (s *Server) showSeason(w http.ResponseWriter, r *http.Request) {
	showID := strings.TrimSpace(r.PathValue("id"))
	seasonNumber, err := strconv.Atoi(r.PathValue("season"))
	if showID == "" || err != nil || seasonNumber <= 0 {
		writeError(w, http.StatusBadRequest, "show ID and positive season number are required")
		return
	}
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.ShowProvider)
	s.metadataMu.RUnlock()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "show metadata is not configured")
		return
	}
	season, err := provider.Season(r.Context(), showID, seasonNumber)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, season)
}

func (s *Server) searchMovies(ctx context.Context, query string) ([]metadata.Movie, error) {
	s.metadataMu.RLock()
	provider := s.metadataProvider
	s.metadataMu.RUnlock()
	if provider != nil {
		movies, err := provider.Search(ctx, query)
		if err != nil {
			return nil, err
		}
		if len(movies) > 0 {
			return movies, nil
		}
	}

	s.resolverMu.RLock()
	movieResolver := s.movieResolver
	s.resolverMu.RUnlock()
	if movieResolver == nil {
		return []metadata.Movie{fallbackMovie(query, 0)}, nil
	}
	result, err := movieResolver.Resolve(ctx, query)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		movies := make([]metadata.Movie, 0, len(result.Candidates))
		for _, candidate := range result.Candidates {
			movies = append(movies, fallbackMovie(candidate.Title, candidate.Year))
		}
		return movies, nil
	}

	movies := make([]metadata.Movie, 0, len(result.Candidates))
	seen := make(map[string]bool)
	for _, candidate := range result.Candidates {
		results, err := provider.Search(ctx, candidate.Title)
		if err != nil {
			return nil, err
		}
		for _, movie := range results {
			if candidate.Year > 0 && movie.Year > 0 && candidate.Year != movie.Year {
				continue
			}
			if !seen[movie.ID] {
				movies = append(movies, movie)
				seen[movie.ID] = true
			}
			break
		}
	}
	return movies, nil
}

func fallbackMovie(title string, year int) metadata.Movie {
	normalized := strings.ToLower(strings.Join(strings.Fields(title), "-"))
	return metadata.Movie{
		ID:        fmt.Sprintf("filmstream:%s:%d", normalized, year),
		MediaType: metadata.MediaTypeMovie,
		Title:     strings.TrimSpace(title),
		Year:      year,
	}
}

func (s *Server) listWatchHistory(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "watch history is not configured")
		return
	}
	entries, err := s.historyStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []history.Entry{}
	}
	entries = s.enrichWatchHistory(r.Context(), entries)
	if r.URL.Query().Get("continue") == "true" {
		entries = s.continueWatchHistory(r.Context(), entries)
	}
	writeJSON(w, http.StatusOK, struct {
		Entries []history.Entry `json:"entries"`
	}{Entries: entries})
}

func (s *Server) continueWatchHistory(ctx context.Context, entries []history.Entry) []history.Entry {
	result := make([]history.Entry, 0, len(entries))
	seenSeries := make(map[string]bool)
	for _, entry := range entries {
		if entry.SeriesID == "" {
			if entry.CanContinue() {
				result = append(result, entry)
			}
			continue
		}
		if seenSeries[entry.SeriesID] {
			continue
		}
		seenSeries[entry.SeriesID] = true

		seriesEntries := make([]history.Entry, 0)
		for _, candidate := range entries {
			if candidate.SeriesID == entry.SeriesID {
				seriesEntries = append(seriesEntries, candidate)
			}
		}
		anchor, ok := latestMeaningfulEpisode(seriesEntries)
		if !ok {
			continue
		}
		if anchor.CanContinue() {
			result = append(result, anchor)
			continue
		}
		if next, ok := s.nextEpisodeAfter(ctx, anchor, seriesEntries); ok {
			result = append(result, next)
		}
	}
	return result
}

func latestMeaningfulEpisode(entries []history.Entry) (history.Entry, bool) {
	var latest history.Entry
	found := false
	for _, entry := range entries {
		if entry.SeasonNumber <= 0 || entry.EpisodeNumber <= 0 || (!entry.Completed && !entry.CanContinue()) {
			continue
		}
		if !found || newerEpisodeActivity(entry, latest) {
			latest = entry
			found = true
		}
	}
	return latest, found
}

func newerEpisodeActivity(candidate, current history.Entry) bool {
	// A newer timestamp is explicit playback intent, including an older episode replay.
	if !candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return candidate.UpdatedAt.After(current.UpdatedAt)
	}
	if comparison := compareEpisodePosition(
		candidate.SeasonNumber, candidate.EpisodeNumber,
		current.SeasonNumber, current.EpisodeNumber,
	); comparison != 0 {
		return comparison > 0
	}
	if candidate.Completed != current.Completed {
		return candidate.Completed
	}
	if candidate.MediaID != current.MediaID {
		return candidate.MediaID < current.MediaID
	}
	return candidate.ID < current.ID
}

func compareEpisodePosition(season, episode, otherSeason, otherEpisode int) int {
	if season != otherSeason {
		return season - otherSeason
	}
	return episode - otherEpisode
}

func (s *Server) nextEpisodeAfter(
	ctx context.Context,
	anchor history.Entry,
	entries []history.Entry,
) (history.Entry, bool) {
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.ShowProvider)
	s.metadataMu.RUnlock()
	if !ok {
		return history.Entry{}, false
	}
	show, err := provider.Show(ctx, anchor.SeriesID)
	if err != nil {
		return history.Entry{}, false
	}
	showID := firstNonEmpty(show.ID, anchor.SeriesID)
	seasonSummaries := append([]metadata.SeasonSummary(nil), show.Seasons...)
	sort.Slice(seasonSummaries, func(i, j int) bool {
		return seasonSummaries[i].Number < seasonSummaries[j].Number
	})
	today := time.Now().UTC().Format(time.DateOnly)
	for _, summary := range seasonSummaries {
		if summary.Number < anchor.SeasonNumber || (summary.AirDate != "" && summary.AirDate > today) {
			continue
		}
		season, err := provider.Season(ctx, showID, summary.Number)
		if err != nil {
			continue
		}
		episodes := append([]metadata.Episode(nil), season.Episodes...)
		sort.Slice(episodes, func(i, j int) bool {
			return compareEpisodePosition(
				episodes[i].SeasonNumber, episodes[i].EpisodeNumber,
				episodes[j].SeasonNumber, episodes[j].EpisodeNumber,
			) < 0
		})
		for _, episode := range episodes {
			if episode.EpisodeNumber <= 0 || (episode.AirDate != "" && episode.AirDate > today) {
				continue
			}
			if compareEpisodePosition(
				episode.SeasonNumber, episode.EpisodeNumber,
				anchor.SeasonNumber, anchor.EpisodeNumber,
			) <= 0 {
				continue
			}
			saved, hasSaved := historyForEpisode(entries, episode)
			if hasSaved && saved.Completed {
				continue
			}
			next := history.Entry{
				ID:              anchor.ID,
				MediaID:         episode.ID,
				MediaType:       string(metadata.MediaTypeShow),
				Title:           show.Title,
				Year:            show.Year,
				Overview:        episode.Overview,
				PosterURL:       show.PosterURL,
				BackdropURL:     firstNonEmpty(episode.StillURL, show.BackdropURL),
				Genres:          show.Genres,
				NumberOfSeasons: show.NumberOfSeasons,
				SeriesID:        showID,
				SeriesTitle:     show.Title,
				SeasonNumber:    episode.SeasonNumber,
				EpisodeNumber:   episode.EpisodeNumber,
				EpisodeTitle:    episode.Title,
				UpdatedAt:       anchor.UpdatedAt,
			}
			if hasSaved && saved.CanContinue() {
				next.PositionSeconds = saved.PositionSeconds
				next.DurationSeconds = saved.DurationSeconds
			}
			return next, true
		}
	}
	return history.Entry{}, false
}

func historyForEpisode(entries []history.Entry, episode metadata.Episode) (history.Entry, bool) {
	var latest history.Entry
	found := false
	for _, entry := range entries {
		matchesID := episode.ID != "" && entry.MediaID == episode.ID
		matchesPosition := entry.SeasonNumber == episode.SeasonNumber && entry.EpisodeNumber == episode.EpisodeNumber
		if !matchesID && !matchesPosition {
			continue
		}
		if !found || newerEpisodeActivity(entry, latest) {
			latest = entry
			found = true
		}
	}
	return latest, found
}

func (s *Server) enrichWatchHistory(ctx context.Context, entries []history.Entry) []history.Entry {
	s.metadataMu.RLock()
	provider := s.metadataProvider
	s.metadataMu.RUnlock()
	if provider == nil {
		return entries
	}
	for i, entry := range entries {
		if (strings.HasPrefix(entry.MediaID, "tmdb:") || strings.HasPrefix(entry.SeriesID, "tmdb-tv:")) && len(entry.Genres) > 0 {
			continue
		}
		searchTitle := entry.Title
		if entry.SeriesTitle != "" {
			searchTitle = entry.SeriesTitle
		}
		movies, err := provider.Search(ctx, searchTitle)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("enrich watch history", "title", entry.Title, "error", err)
			}
			continue
		}
		for _, movie := range movies {
			expectedID := entry.MediaID
			if entry.SeriesID != "" {
				expectedID = entry.SeriesID
			}
			hasTMDBID := strings.HasPrefix(expectedID, "tmdb:") || strings.HasPrefix(expectedID, "tmdb-tv:")
			if hasTMDBID && movie.ID != expectedID {
				continue
			}
			if !hasTMDBID && entry.Year > 0 && movie.Year > 0 && entry.Year != movie.Year {
				continue
			}
			enriched, err := s.historyStore.UpdateMetadata(entry.ID, history.Entry{
				MediaID:         firstNonEmpty(entry.MediaID, movie.ID),
				MediaType:       string(movie.MediaType),
				SeriesID:        entry.SeriesID,
				SeriesTitle:     entry.SeriesTitle,
				NumberOfSeasons: movie.NumberOfSeasons,
				Overview:        movie.Overview,
				PosterURL:       movie.PosterURL,
				BackdropURL:     movie.BackdropURL,
				Genres:          movie.Genres,
			})
			if err == nil {
				entries[i] = enriched
			} else if s.logger != nil {
				s.logger.Warn("save watch history metadata", "title", entry.Title, "error", err)
			}
			break
		}
	}
	return entries
}

func (s *Server) removeWatchHistory(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "watch history is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "watch history ID cannot be empty")
		return
	}
	seriesID := strings.TrimSpace(r.URL.Query().Get("series_id"))
	var err error
	if seriesID != "" {
		err = s.historyStore.RemoveSeries(seriesID)
	} else {
		err = s.historyStore.Remove(id)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) updateWatchProgress(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "watch history is not configured")
		return
	}
	defer r.Body.Close()
	var request struct {
		MediaID           string                    `json:"media_id,omitempty"`
		MediaType         string                    `json:"media_type,omitempty"`
		Title             string                    `json:"title"`
		Year              int                       `json:"year,omitempty"`
		Overview          string                    `json:"overview,omitempty"`
		PosterURL         string                    `json:"poster_url,omitempty"`
		BackdropURL       string                    `json:"backdrop_url,omitempty"`
		Genres            []string                  `json:"genres,omitempty"`
		NumberOfSeasons   int                       `json:"number_of_seasons,omitempty"`
		SeriesID          string                    `json:"series_id,omitempty"`
		SeriesTitle       string                    `json:"series_title,omitempty"`
		SeasonNumber      int                       `json:"season_number,omitempty"`
		EpisodeNumber     int                       `json:"episode_number,omitempty"`
		EpisodeTitle      string                    `json:"episode_title,omitempty"`
		PositionSeconds   float64                   `json:"position_seconds"`
		DurationSeconds   float64                   `json:"duration_seconds,omitempty"`
		SubtitleSelection playbackSubtitleSelection `json:"subtitle_selection,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	subtitleSelection, err := normalizedSubtitleSelection(request.SubtitleSelection)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entry, err := s.historyStore.RecordProgress(history.Entry{
		MediaID:         request.MediaID,
		MediaType:       request.MediaType,
		Title:           request.Title,
		Year:            request.Year,
		Overview:        request.Overview,
		PosterURL:       request.PosterURL,
		BackdropURL:     request.BackdropURL,
		Genres:          request.Genres,
		NumberOfSeasons: request.NumberOfSeasons,
		SeriesID:        request.SeriesID,
		SeriesTitle:     request.SeriesTitle,
		SeasonNumber:    request.SeasonNumber,
		EpisodeNumber:   request.EpisodeNumber,
		EpisodeTitle:    request.EpisodeTitle,
		PositionSeconds: request.PositionSeconds,
		DurationSeconds: request.DurationSeconds,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if entry.SeriesID != "" {
		go s.queueNextEpisodePrewarm(entry, subtitleSelection)
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) reloadResolverConfiguration(w http.ResponseWriter, _ *http.Request) {
	if s.reloadResolver == nil {
		writeError(w, http.StatusNotImplemented, "resolver reload is not configured")
		return
	}
	if err := s.reloadResolver(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) reloadIndexerConfiguration(w http.ResponseWriter, _ *http.Request) {
	if s.reloadIndexers == nil {
		writeError(w, http.StatusNotImplemented, "indexer reload is not configured")
		return
	}
	if err := s.reloadIndexers(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) createPlayback(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request CreatePlaybackRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	request.MediaID = strings.TrimSpace(request.MediaID)
	request.MediaType = strings.TrimSpace(request.MediaType)
	request.Query = strings.TrimSpace(request.Query)
	request.OriginalTitle = strings.TrimSpace(request.OriginalTitle)
	request.SeriesID = strings.TrimSpace(request.SeriesID)
	request.SeriesTitle = strings.TrimSpace(request.SeriesTitle)
	request.EpisodeTitle = strings.TrimSpace(request.EpisodeTitle)
	if request.StartSeconds < 0 {
		writeError(w, http.StatusBadRequest, "start_seconds cannot be negative")
		return
	}
	if request.SeasonNumber > 0 || request.EpisodeNumber > 0 {
		if request.SeasonNumber <= 0 || request.EpisodeNumber <= 0 || request.SeriesID == "" {
			writeError(w, http.StatusBadRequest, "episode playback requires series_id, season_number, and episode_number")
			return
		}
		request.MediaType = "show"
		if request.Query == "" {
			request.Query = request.SeriesTitle
		}
	}

	preferences := mergePreferences(s.defaults, request.Preferences)
	request.Preferences = preferences
	sourceCount := 0
	if request.Query != "" {
		sourceCount++
	}
	if request.MagnetURI != "" {
		sourceCount++
	}
	if request.TorrentPath != "" {
		sourceCount++
	}
	if sourceCount != 1 {
		writeError(w, http.StatusBadRequest, "provide exactly one of query, magnet_uri, or torrent_path")
		return
	}
	if r.Header.Get(prewarmRequestHeader) == "" {
		if response, ok := s.claimPrewarmedPlayback(r.Context(), request); ok {
			response.StreamURL = fmt.Sprintf("%s://%s/v1/playbacks/%s/stream", requestScheme(r), r.Host, response.ID)
			writeJSON(w, http.StatusCreated, response)
			return
		}
	}

	var session *playbackSession
	var selected *catalog.RankedCandidate
	if request.Query != "" {
		isShow := request.MediaType == "show"
		preferSeasonPack := isShow && s.shouldPreferSeasonPack(r.Context(), request)
		search := catalog.SearchRequest{
			Query: request.Query, Year: request.Year, MediaType: request.MediaType,
			SeasonNumber: request.SeasonNumber, EpisodeNumber: request.EpisodeNumber,
			PreferSeasonPack: preferSeasonPack,
			Preferences:      preferences,
		}
		allowUsenet := !isShow && s.playbackSourceMode != config.PlaybackSourceTorrentOnly
		allowTorrent := isShow || s.playbackSourceMode != config.PlaybackSourceUsenetOnly
		var ranked []catalog.RankedCandidate
		var searchErr error
		var usenetErr error

		if allowUsenet {
			if s.usenetEngine == nil {
				if !allowTorrent {
					writeError(w, http.StatusServiceUnavailable, "Usenet-only playback is configured but the Usenet engine is unavailable")
					return
				}
			} else {
				session, selected = s.createCachedUsenetPlayback(r.Context(), request, preferences)
				if session == nil {
					ranked, searchErr = s.searchAndRank(
						r.Context(), search, request.OriginalTitle, catalog.ProtocolUsenet,
					)
					if searchErr == nil {
						session, selected, usenetErr = s.createRankedUsenetPlayback(
							r.Context(), ranked, preferences, playbackFileHint(request),
						)
						if session == nil {
							ranked = nil
						}
					}
				}
			}
		}

		if session == nil && !allowTorrent {
			switch {
			case searchErr != nil:
				writeError(w, http.StatusBadGateway, searchErr.Error())
			case usenetErr != nil:
				s.logger.Warn("Usenet playback unavailable", "title", request.Query,
					"media_id", request.MediaID, "error", usenetErr)
				writeError(w, http.StatusBadGateway,
					"Usenet playback unavailable: no playable release passed validation. Please try again later.")
			default:
				writeError(w, http.StatusNotFound, "no matching Usenet candidates found")
			}
			return
		}

		if session == nil {
			var err error
			session, selected, err = s.createCachedPlayback(r.Context(), request)
			if err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			if session != nil && usenetErr != nil {
				s.logger.Warn("Usenet playback unavailable; using cached torrent", "title", request.Query, "error", usenetErr)
			}
		}
		if session == nil {
			if ranked == nil && searchErr == nil {
				if isShow {
					if cachedRanked, found := s.cachedShowReleases(r.Context(), request); found {
						ranked = cachedRanked
						s.logger.Info("reused prefetched TV release search", "media_id", request.MediaID,
							"candidates", len(ranked))
					}
				}
				if ranked == nil {
					protocol := ""
					if isShow {
						protocol = catalog.ProtocolTorrent
					}
					ranked, searchErr = s.searchAndRank(r.Context(), search, request.OriginalTitle, protocol)
				}
			}
			if searchErr != nil {
				writeError(w, http.StatusBadGateway, searchErr.Error())
				return
			}
			if len(ranked) == 0 {
				writeError(w, http.StatusNotFound, "no matching streaming candidates found")
				return
			}
			torrentSession, torrentSelected, err := s.createRankedPlayback(
				r.Context(), ranked, preferences, playbackFileHint(request),
			)
			if err != nil {
				if usenetErr != nil {
					err = fmt.Errorf("Usenet candidates failed: %v; torrent fallback failed: %w", usenetErr, err)
				}
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			session = wrapTorrentSession(torrentSession)
			selected = torrentSelected
			if usenetErr != nil {
				s.logger.Warn("Usenet playback unavailable; using torrent fallback", "title", request.Query, "error", usenetErr)
			}
		}
	} else {
		torrentSession, err := s.engine.Create(r.Context(), torrentstream.Source{
			MagnetURI: request.MagnetURI, TorrentPath: request.TorrentPath,
			FileHint: playbackFileHint(request),
		})
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		session = wrapTorrentSession(torrentSession)
	}
	s.mu.Lock()
	if s.playbackLanguages == nil {
		s.playbackLanguages = make(map[string][]string)
	}
	s.playbackLanguages[session.ID] = append([]string(nil), preferences.Languages...)
	s.mu.Unlock()
	if selected != nil {
		public := publicRankedCandidate(*selected)
		selected = &public
		s.mu.Lock()
		s.selected[session.ID] = public
		if request.Query != "" {
			s.playbackCacheKeys[session.ID] = playbackCacheKey{
				mediaID: playbackCacheMediaID(request, session.Source, selected),
				title:   request.Query,
				year:    request.Year,
				source:  session.Source,
			}
		}
		s.mu.Unlock()
	}

	response := CreatePlaybackResponse{
		ID:        session.ID,
		Name:      session.Name,
		FileName:  session.FileName,
		FileSize:  session.FileSize,
		Source:    session.Source,
		StreamURL: fmt.Sprintf("%s://%s/v1/playbacks/%s/stream", requestScheme(r), r.Host, session.ID),
		Selected:  selected,
	}
	s.mu.Lock()
	s.playbackRequests[session.ID] = request
	s.playbackResponses[session.ID] = response
	s.mu.Unlock()
	s.logger.Info("created playback", "id", session.ID, "source", session.Source,
		"media_id", request.MediaID, "season", request.SeasonNumber, "episode", request.EpisodeNumber,
		"name", session.Name, "file", session.FileName)
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) searchAndRank(
	ctx context.Context,
	request catalog.SearchRequest,
	originalTitle string,
	protocol string,
) ([]catalog.RankedCandidate, error) {
	titles := []string{request.Query}
	if originalTitle = strings.TrimSpace(originalTitle); originalTitle != "" && !strings.EqualFold(originalTitle, request.Query) {
		titles = append(titles, originalTitle)
	}
	var failures []error
	hadSuccessfulSearch := false
	var rankedCandidates []catalog.RankedCandidate
	seenCandidates := make(map[string]bool)
	for _, title := range titles {
		search := request
		search.Query = title
		var candidates []catalog.Candidate
		var err error
		if protocol == "" {
			candidates, err = s.indexers.Search(ctx, search)
		} else if search.MediaType == "show" && protocol == catalog.ProtocolTorrent {
			candidates, err = s.indexers.SearchProtocol(ctx, search, protocol)
		} else {
			candidates, err = s.indexers.SearchFirstProtocol(ctx, search, protocol)
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		hadSuccessfulSearch = true
		for _, ranked := range catalog.Rank(search, candidates) {
			key := ranked.Candidate.Indexer + ":" + firstNonEmpty(ranked.Candidate.ID, ranked.Candidate.Name)
			if seenCandidates[key] {
				continue
			}
			seenCandidates[key] = true
			rankedCandidates = append(rankedCandidates, ranked)
		}
	}
	if !hadSuccessfulSearch && len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	sort.SliceStable(rankedCandidates, func(i, j int) bool {
		return rankedCandidates[i].Score > rankedCandidates[j].Score
	})
	return rankedCandidates, nil
}

func wrapTorrentSession(session *torrentstream.Session) *playbackSession {
	return &playbackSession{
		ID: session.ID, Name: session.Name, FileName: session.FileName,
		FileSize: session.FileSize, Source: catalog.ProtocolTorrent,
	}
}

func wrapUsenetSession(session *usenetstream.Session) *playbackSession {
	return &playbackSession{
		ID: session.ID, Name: session.Name, FileName: session.FileName,
		FileSize: session.FileSize, Source: catalog.ProtocolUsenet,
	}
}

type playbackOption struct {
	session          *torrentstream.Session
	candidate        catalog.RankedCandidate
	peers            int
	subtitlesChecked bool
}

func (s *Server) createCachedUsenetPlayback(
	ctx context.Context,
	request CreatePlaybackRequest,
	preferences catalog.Preferences,
) (*playbackSession, *catalog.RankedCandidate) {
	if s.playbackCache == nil || s.usenetEngine == nil {
		return nil, nil
	}
	cached, found, err := s.playbackCache.LookupUsenet(request.MediaID, request.Query, request.Year)
	if err != nil {
		s.logger.Warn("load cached Usenet playback selection", "title", request.Query, "error", err)
		return nil, nil
	}
	if !found {
		return nil, nil
	}
	if cached.Selected.Candidate.Protocol != catalog.ProtocolUsenet {
		s.logger.Warn("remove invalid cached Usenet playback selection", "title", request.Query)
		if removeErr := s.playbackCache.RemoveUsenet(request.MediaID, request.Query, request.Year); removeErr != nil {
			s.logger.Warn("remove invalid cached Usenet playback selection", "title", request.Query, "error", removeErr)
		}
		return nil, nil
	}

	preparationContext, cancel := context.WithTimeout(ctx, maxUsenetPreparation)
	defer cancel()
	session, err := s.usenetEngine.Create(preparationContext, usenetstream.Source{
		NZBPath:  cached.NZBPath,
		Name:     cached.Selected.Candidate.Name,
		FileHint: playbackFileHint(request),
	})
	if err != nil {
		transient := transientPlaybackFailure(err)
		if ctx.Err() == nil && !transient {
			s.markUsenetCandidateFailed(cached.Selected.Candidate, playbackFileHint(request))
		}
		if !transient {
			if removeErr := s.playbackCache.RemoveUsenet(request.MediaID, request.Query, request.Year); removeErr != nil {
				s.logger.Warn("remove unusable cached NZB", "title", request.Query, "error", removeErr)
			}
		}
		s.logger.Warn("cached Usenet playback is unavailable; searching again",
			"title", request.Query, "name", cached.Selected.Candidate.Name, "error", err)
		return nil, nil
	}
	if preferences.PreferTextSubtitles {
		hasSubtitles, probeErr := s.playbackHasSubtitles(preparationContext, session.ID)
		if probeErr != nil {
			_ = s.usenetEngine.Drop(session.ID)
			if !transientPlaybackFailure(probeErr) {
				s.markUsenetCandidateFailed(cached.Selected.Candidate, playbackFileHint(request))
				if removeErr := s.playbackCache.RemoveUsenet(request.MediaID, request.Query, request.Year); removeErr != nil {
					s.logger.Warn("remove cached NZB that failed media probing", "title", request.Query, "error", removeErr)
				}
			}
			s.logger.Warn("cached Usenet playback failed media probing; searching again",
				"title", request.Query, "name", cached.Selected.Candidate.Name, "error", probeErr)
			return nil, nil
		}
		if !hasSubtitles {
			_ = s.usenetEngine.Drop(session.ID)
			if removeErr := s.playbackCache.RemoveUsenet(request.MediaID, request.Query, request.Year); removeErr != nil {
				s.logger.Warn("remove cached NZB without supported subtitles", "title", request.Query, "error", removeErr)
			}
			s.logger.Info("cached Usenet playback lacks supported subtitles; searching again", "title", request.Query)
			return nil, nil
		}
	}
	s.clearUsenetCandidateFailure(cached.Selected.Candidate, playbackFileHint(request))
	s.logger.Info("reused cached Usenet playback selection", "id", session.ID,
		"name", cached.Selected.Candidate.Name)
	selected := cached.Selected
	return wrapUsenetSession(session), &selected
}

func (s *Server) createCachedPlayback(
	ctx context.Context,
	request CreatePlaybackRequest,
) (*playbackSession, *catalog.RankedCandidate, error) {
	if s.playbackCache == nil {
		return nil, nil, nil
	}
	cacheMediaIDs := []string{request.MediaID}
	if seasonMediaID := seasonPlaybackCacheMediaID(request); seasonMediaID != "" && seasonMediaID != request.MediaID {
		cacheMediaIDs = append([]string{seasonMediaID}, cacheMediaIDs...)
	}
	var cached playbackcache.Entry
	cacheMediaID := ""
	for _, mediaID := range cacheMediaIDs {
		entry, found, err := s.playbackCache.Lookup(mediaID, request.Query, request.Year)
		if err != nil {
			s.logger.Warn("load cached playback selection", "title", request.Query, "error", err)
			return nil, nil, nil
		}
		if found {
			cached = entry
			cacheMediaID = mediaID
			break
		}
	}
	if cacheMediaID == "" {
		return nil, nil, nil
	}
	if request.Preferences.PreferTextSubtitles &&
		!hasRankingReason(cached.Selected, subtitlesVerifiedReason) &&
		!hasRankingReason(cached.Selected, subtitleFallbackReason) {
		if removeErr := s.playbackCache.Remove(cacheMediaID, request.Query, request.Year); removeErr != nil {
			s.logger.Warn("remove cached playback without subtitle validation", "title", request.Query, "error", removeErr)
		}
		s.logger.Info("cached playback predates subtitle validation", "title", request.Query,
			"name", cached.Selected.Candidate.Name)
		return nil, nil, nil
	}
	cachedSearch := catalog.SearchRequest{
		Query: request.Query, Year: request.Year, MediaType: request.MediaType,
		SeasonNumber: request.SeasonNumber, EpisodeNumber: request.EpisodeNumber,
		PreferSeasonPack: request.MediaType == string(metadata.MediaTypeShow) && s.shouldPreferSeasonPack(ctx, request),
		Preferences:      request.Preferences,
	}
	if len(catalog.Rank(cachedSearch, []catalog.Candidate{cached.Selected.Candidate})) == 0 {
		if removeErr := s.playbackCache.Remove(cacheMediaID, request.Query, request.Year); removeErr != nil {
			s.logger.Warn("remove cached playback rejected by current policy", "title", request.Query, "error", removeErr)
		}
		s.logger.Info("cached playback no longer matches release policy", "title", request.Query,
			"name", cached.Selected.Candidate.Name)
		return nil, nil, nil
	}
	session, err := s.engine.Create(ctx, torrentstream.Source{
		TorrentPath: cached.TorrentPath, FileHint: playbackFileHint(request),
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		s.logger.Warn("start cached playback selection", "title", request.Query, "error", err)
		if removeErr := s.playbackCache.Remove(cacheMediaID, request.Query, request.Year); removeErr != nil {
			s.logger.Warn("remove unusable playback cache", "title", request.Query, "error", removeErr)
		}
		return nil, nil, nil
	}
	options := []playbackOption{{session: session, candidate: cached.Selected}}
	strong, err := s.observePlaybackOptions(ctx, options, minimumLivePeers, cachedSwarmWait)
	if err != nil {
		_ = s.engine.Drop(session.ID)
		return nil, nil, err
	}
	if strong < 0 {
		_ = s.engine.Drop(session.ID)
		if removeErr := s.playbackCache.Remove(cacheMediaID, request.Query, request.Year); removeErr != nil {
			s.logger.Warn("remove unavailable playback cache", "title", request.Query, "error", removeErr)
		}
		s.logger.Warn("cached playback swarm is unavailable; searching again",
			"title", request.Query, "live_peers", options[0].peers)
		return nil, nil, nil
	}
	// A cached release has already produced a valid HLS stream. Re-probing it here
	// downloads media before playback and turns every background lookup into torrent
	// payload work. The HLS startup probe will expose this episode's actual tracks.
	s.logger.Info("reused cached playback selection", "id", session.ID,
		"name", cached.Selected.Candidate.Name, "live_peers", options[strong].peers)
	selected := cached.Selected
	return wrapTorrentSession(session), &selected, nil
}

func (s *Server) cacheSuccessfulPlayback(id string) {
	if s.playbackCache == nil {
		return
	}
	s.mu.RLock()
	key, hasKey := s.playbackCacheKeys[id]
	selected, hasSelection := s.selected[id]
	s.mu.RUnlock()
	if !hasKey || !hasSelection {
		return
	}

	switch key.source {
	case catalog.ProtocolUsenet:
		if s.usenetEngine == nil {
			return
		}
		contents, err := s.usenetEngine.NZB(id)
		if err != nil {
			s.logger.Warn("export selected NZB", "title", key.title, "error", err)
			return
		}
		if _, err := s.playbackCache.SaveUsenet(key.mediaID, key.title, key.year, selected, contents); err != nil {
			s.logger.Warn("cache Usenet playback selection", "title", key.title, "error", err)
		}
	default:
		contents, err := s.engine.TorrentMetainfo(id)
		if err != nil {
			s.logger.Warn("export selected torrent metadata", "title", key.title, "error", err)
			return
		}
		if _, err := s.playbackCache.Save(key.mediaID, key.title, key.year, selected, contents); err != nil {
			s.logger.Warn("cache playback selection", "title", key.title, "error", err)
		}
	}
}

func (s *Server) invalidateCachedPlayback(id string) {
	if s.playbackCache == nil {
		return
	}
	s.mu.RLock()
	key, ok := s.playbackCacheKeys[id]
	selected, hasSelection := s.selected[id]
	request := s.playbackRequests[id]
	s.mu.RUnlock()
	if !ok {
		return
	}
	var err error
	if key.source == catalog.ProtocolUsenet {
		if hasSelection {
			s.markUsenetCandidateFailed(selected.Candidate, playbackFileHint(request))
		}
		err = s.playbackCache.RemoveUsenet(key.mediaID, key.title, key.year)
	} else {
		err = s.playbackCache.Remove(key.mediaID, key.title, key.year)
	}
	if err != nil {
		s.logger.Warn("invalidate playback cache", "title", key.title, "source", key.source, "error", err)
	}
}

func (s *Server) createRankedUsenetPlayback(
	ctx context.Context,
	ranked []catalog.RankedCandidate,
	preferences catalog.Preferences,
	fileHint string,
) (*playbackSession, *catalog.RankedCandidate, error) {
	if s.usenetEngine == nil {
		return nil, nil, nil
	}
	preparationContext, cancel := context.WithTimeout(ctx, maxUsenetPreparation)
	defer cancel()
	attempts := 0
	var failures []string
	var fallbackSession *usenetstream.Session
	var fallbackCandidate *catalog.RankedCandidate
	for _, candidate := range ranked {
		if candidate.Candidate.Protocol != catalog.ProtocolUsenet && candidate.Candidate.NZBURL == "" {
			continue
		}
		if s.usenetCandidateRecentlyFailed(candidate.Candidate, fileHint) {
			failures = append(failures, candidate.Candidate.Name+": recently failed validation")
			continue
		}
		if attempts >= maxUsenetCandidates {
			break
		}
		attempts++
		resolved, err := s.indexers.Resolve(preparationContext, candidate.Candidate)
		if err == nil {
			var session *usenetstream.Session
			session, err = s.usenetEngine.Create(preparationContext, usenetstream.Source{
				NZBURL: resolved.NZBURL, Name: candidate.Candidate.Name, FileHint: fileHint,
			})
			if err == nil {
				s.clearUsenetCandidateFailure(candidate.Candidate, fileHint)
				if !preferences.PreferTextSubtitles {
					s.logger.Info("selected Usenet playback", "id", session.ID, "name", candidate.Candidate.Name)
					return wrapUsenetSession(session), &candidate, nil
				}
				hasSubtitles, probeErr := s.playbackHasSubtitles(preparationContext, session.ID)
				if probeErr != nil {
					_ = s.usenetEngine.Drop(session.ID)
					if !transientPlaybackFailure(probeErr) {
						s.markUsenetCandidateFailed(candidate.Candidate, fileHint)
					}
					failures = append(failures, fmt.Sprintf("%s: probe media: %v", candidate.Candidate.Name, probeErr))
					s.logger.Warn("reject Usenet playback that failed media probing", "id", session.ID,
						"name", candidate.Candidate.Name, "error", probeErr)
					continue
				}
				if hasSubtitles {
					if fallbackSession != nil {
						_ = s.usenetEngine.Drop(fallbackSession.ID)
					}
					s.logger.Info("selected Usenet playback with supported subtitles",
						"id", session.ID, "name", candidate.Candidate.Name)
					return wrapUsenetSession(session), &candidate, nil
				}
				if fallbackSession == nil {
					fallbackSession = session
					fallback := candidate
					fallbackCandidate = &fallback
				} else {
					_ = s.usenetEngine.Drop(session.ID)
				}
				continue
			}
		}
		if preparationContext.Err() != nil {
			if ctx.Err() != nil {
				if fallbackSession != nil {
					_ = s.usenetEngine.Drop(fallbackSession.ID)
				}
				return nil, nil, ctx.Err()
			}
			if fallbackSession != nil {
				break
			}
			return nil, nil, fmt.Errorf("Usenet preparation exceeded %s", maxUsenetPreparation)
		}
		if !transientPlaybackFailure(err) {
			s.markUsenetCandidateFailed(candidate.Candidate, fileHint)
		}
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.Candidate.Name, err))
	}
	if fallbackSession != nil {
		s.logger.Warn("no Usenet candidate exposed supported subtitles; using best available release",
			"id", fallbackSession.ID, "name", fallbackCandidate.Candidate.Name)
		return wrapUsenetSession(fallbackSession), fallbackCandidate, nil
	}
	if len(failures) > 0 {
		return nil, nil, errors.New(strings.Join(failures, "; "))
	}
	return nil, nil, nil
}

func transientPlaybackFailure(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Server) usenetCandidateRecentlyFailed(candidate catalog.Candidate, fileHint ...string) bool {
	key := usenetCandidateKey(candidate, fileHint...)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, found := s.usenetFailures[key]
	if found && now.After(expiresAt) {
		delete(s.usenetFailures, key)
		return false
	}
	return found
}

func (s *Server) markUsenetCandidateFailed(candidate catalog.Candidate, fileHint ...string) {
	s.mu.Lock()
	if s.usenetFailures == nil {
		s.usenetFailures = make(map[string]time.Time)
	}
	s.usenetFailures[usenetCandidateKey(candidate, fileHint...)] = time.Now().Add(usenetFailureTTL)
	failures := copyUsenetFailures(s.usenetFailures)
	store := s.playbackCache
	s.mu.Unlock()
	s.persistUsenetFailures(store, failures)
}

func (s *Server) clearUsenetCandidateFailure(candidate catalog.Candidate, fileHint ...string) {
	s.mu.Lock()
	delete(s.usenetFailures, usenetCandidateKey(candidate, fileHint...))
	failures := copyUsenetFailures(s.usenetFailures)
	store := s.playbackCache
	s.mu.Unlock()
	s.persistUsenetFailures(store, failures)
}

func (s *Server) persistUsenetFailures(store *playbackcache.Store, failures map[string]time.Time) {
	if store == nil {
		return
	}
	if err := store.SaveUsenetFailures(failures); err != nil {
		s.logger.Warn("save Usenet failure cache", "error", err)
	}
}

func copyUsenetFailures(failures map[string]time.Time) map[string]time.Time {
	result := make(map[string]time.Time, len(failures))
	for key, expiresAt := range failures {
		result[key] = expiresAt
	}
	return result
}

func usenetCandidateKey(candidate catalog.Candidate, fileHint ...string) string {
	key := candidate.Indexer + ":" + candidate.Name
	if candidate.ID != "" {
		key = candidate.Indexer + ":" + candidate.ID
	}
	if len(fileHint) > 0 && fileHint[0] != "" {
		key += ":" + strings.ToLower(fileHint[0])
	}
	return key
}

func (s *Server) createRankedPlayback(
	ctx context.Context,
	ranked []catalog.RankedCandidate,
	preferences catalog.Preferences,
	fileHint string,
) (*torrentstream.Session, *catalog.RankedCandidate, error) {
	torrents := make([]catalog.RankedCandidate, 0, len(ranked))
	for _, candidate := range ranked {
		if candidate.Candidate.Protocol == catalog.ProtocolUsenet || candidate.Candidate.NZBURL != "" {
			continue
		}
		torrents = append(torrents, candidate)
	}
	if len(torrents) == 0 {
		return nil, nil, errors.New("no usable torrent candidates found")
	}
	ranked = torrents
	attempts := 1
	if preferences.StreamingOptimized {
		attempts = min(maxLiveSwarmCandidates, len(ranked))
	}
	timing := s.rankedPlaybackTiming()
	options := make([]playbackOption, 0, attempts)
	observationRemaining := timing.liveSwarmWait
	var failures []string

	// Peer discovery continues only while a torrent is mounted. Keep an unsuccessful
	// option alive as a possible late fallback, but mount the next release only after
	// the current one lacks subtitles or misses its short health window.
	// choosePlaybackOption drops every unselected mount as soon as one release works.
	for i := 0; i < attempts; i++ {
		candidate := ranked[i]
		resolved, err := s.indexers.Resolve(ctx, candidate.Candidate)
		if err != nil {
			if ctx.Err() != nil {
				s.dropPlaybackOptions(options)
				return nil, nil, ctx.Err()
			}
			failures = append(failures, fmt.Sprintf("%s: %v", candidate.Candidate.Name, err))
			continue
		}
		session, err := s.engine.Create(ctx, torrentstream.Source{
			MagnetURI: resolved.MagnetURI, TorrentURL: resolved.TorrentURL, FileHint: fileHint,
		})
		if err != nil {
			if ctx.Err() != nil {
				s.dropPlaybackOptions(options)
				return nil, nil, ctx.Err()
			}
			failures = append(failures, fmt.Sprintf("%s: %v", candidate.Candidate.Name, err))
			continue
		}
		if !preferences.StreamingOptimized {
			return session, &candidate, nil
		}

		current := len(options)
		options = append(options, playbackOption{session: session, candidate: candidate})
		wait := time.Duration(0)
		if i+1 < attempts {
			wait = min(timing.progressiveWait, observationRemaining)
		}
		started := time.Now()
		if preferences.PreferTextSubtitles {
			subtitleOption, err := s.observeSubtitlePlaybackOptions(ctx, options, current, wait)
			observationRemaining -= min(time.Since(started), observationRemaining)
			if err != nil {
				s.dropPlaybackOptions(options)
				return nil, nil, err
			}
			if subtitleOption >= 0 {
				options[subtitleOption].candidate.Reasons = append(
					options[subtitleOption].candidate.Reasons,
					subtitlesVerifiedReason,
				)
				return s.choosePlaybackOption(
					options,
					subtitleOption,
					options[subtitleOption].peers >= strongLiveSwarmPeers,
				)
			}
			continue
		}

		strong, err := s.observePlaybackOptions(ctx, options, strongLiveSwarmPeers, wait)
		observationRemaining -= min(time.Since(started), observationRemaining)
		if err != nil {
			s.dropPlaybackOptions(options)
			return nil, nil, err
		}
		if strong >= 0 {
			return s.choosePlaybackOption(options, strong, true)
		}
	}
	if len(options) == 0 {
		if len(failures) > 0 {
			return nil, nil, errors.New(strings.Join(failures, "; "))
		}
		return nil, nil, errors.New("no usable torrent candidates found")
	}

	if preferences.PreferTextSubtitles {
		started := time.Now()
		subtitleOption, err := s.observeSubtitlePlaybackOptions(ctx, options, -1, observationRemaining)
		observationRemaining -= min(time.Since(started), observationRemaining)
		if err != nil {
			s.dropPlaybackOptions(options)
			return nil, nil, err
		}
		if subtitleOption >= 0 {
			options[subtitleOption].candidate.Reasons = append(
				options[subtitleOption].candidate.Reasons,
				subtitlesVerifiedReason,
			)
			return s.choosePlaybackOption(
				options,
				subtitleOption,
				options[subtitleOption].peers >= strongLiveSwarmPeers,
			)
		}
	}

	strong, err := s.observePlaybackOptions(ctx, options, strongLiveSwarmPeers, observationRemaining)
	if err != nil {
		s.dropPlaybackOptions(options)
		return nil, nil, err
	}
	if preferences.PreferTextSubtitles {
		s.logger.Warn("no live ranked playback option exposed supported subtitles; using best available release")
		if strong >= 0 {
			options[strong].candidate.Reasons = append(options[strong].candidate.Reasons, subtitleFallbackReason)
		}
	}
	if strong >= 0 {
		return s.choosePlaybackOption(options, strong, true)
	}
	best := 0
	for i := 1; i < len(options); i++ {
		if options[i].peers > options[best].peers {
			best = i
		}
	}
	if preferences.PreferTextSubtitles {
		options[best].candidate.Reasons = append(options[best].candidate.Reasons, subtitleFallbackReason)
	}
	return s.choosePlaybackOption(options, best, false)
}

func (s *Server) rankedPlaybackTiming() playbackSelectionTiming {
	timing := s.selectionTiming
	if timing.progressiveWait <= 0 {
		timing.progressiveWait = progressiveSwarmWait
	}
	if timing.liveSwarmWait <= 0 {
		timing.liveSwarmWait = liveSwarmWait
	}
	if timing.statusPoll <= 0 {
		timing.statusPoll = swarmStatusPollInterval
	}
	return timing
}

func hasRankingReason(candidate catalog.RankedCandidate, reason string) bool {
	for _, candidateReason := range candidate.Reasons {
		if candidateReason == reason {
			return true
		}
	}
	return false
}

func (s *Server) playbackHasSubtitles(ctx context.Context, playbackID string) (bool, error) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		return false, errors.New("HLS playback is not configured")
	}
	tracks, err := manager.ProbeSubtitles(ctx, playbackID)
	if err != nil {
		return false, err
	}
	return len(tracks) > 0, nil
}

// observeSubtitlePlaybackOptions probes each option once, and only after it has
// enough live peers to be safe. current identifies the newly mounted option: once
// that option is checked, its caller can immediately progress instead of idling for
// the rest of the health window. A negative current performs the final fallback wait.
func (s *Server) observeSubtitlePlaybackOptions(
	ctx context.Context,
	options []playbackOption,
	current int,
	wait time.Duration,
) (int, error) {
	check := func() (int, bool, error) {
		s.refreshPlaybackOptions(options)
		for index := range options {
			option := &options[index]
			if option.peers < minimumLivePeers || option.subtitlesChecked {
				continue
			}
			option.subtitlesChecked = true
			hasSubtitles, err := s.playbackHasSubtitles(ctx, option.session.ID)
			if err != nil {
				if ctx.Err() != nil {
					return -1, false, ctx.Err()
				}
				s.logger.Warn("probe playback subtitles", "id", option.session.ID,
					"name", option.candidate.Candidate.Name, "error", err)
				continue
			}
			if hasSubtitles {
				s.logger.Info("preferred playback with supported subtitles", "id", option.session.ID,
					"name", option.candidate.Candidate.Name, "live_peers", option.peers)
				return index, true, nil
			}
		}
		if current >= 0 && options[current].subtitlesChecked {
			return -1, true, nil
		}
		if current < 0 {
			allChecked := true
			for index := range options {
				allChecked = allChecked && options[index].subtitlesChecked
			}
			if allChecked {
				return -1, true, nil
			}
		}
		return -1, false, nil
	}
	if selected, done, err := check(); err != nil || done || wait <= 0 {
		return selected, err
	}
	timer := time.NewTimer(wait)
	ticker := time.NewTicker(s.rankedPlaybackTiming().statusPoll)
	defer timer.Stop()
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-timer.C:
			selected, _, err := check()
			return selected, err
		case <-ticker.C:
			if selected, done, err := check(); err != nil || done {
				return selected, err
			}
		}
	}
}

func (s *Server) observePlaybackOptions(
	ctx context.Context,
	options []playbackOption,
	threshold int,
	wait time.Duration,
) (int, error) {
	refresh := func() int {
		s.refreshPlaybackOptions(options)
		for i := range options {
			if options[i].peers >= threshold {
				return i
			}
		}
		return -1
	}
	if strong := refresh(); strong >= 0 || wait <= 0 {
		return strong, nil
	}
	timer := time.NewTimer(wait)
	ticker := time.NewTicker(s.rankedPlaybackTiming().statusPoll)
	defer timer.Stop()
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-timer.C:
			return refresh(), nil
		case <-ticker.C:
			if strong := refresh(); strong >= 0 {
				return strong, nil
			}
		}
	}
}

func (s *Server) refreshPlaybackOptions(options []playbackOption) {
	for i := range options {
		status, ok := s.engine.Status(options[i].session.ID)
		if ok {
			options[i].peers = max(status.ActivePeers, status.ConnectedSeeders)
		}
	}
}

func (s *Server) choosePlaybackOption(
	options []playbackOption,
	chosenIndex int,
	strong bool,
) (*torrentstream.Session, *catalog.RankedCandidate, error) {
	chosen := options[chosenIndex]
	for i := range options {
		if i != chosenIndex {
			_ = s.engine.Drop(options[i].session.ID)
		}
	}
	reportedSeeders := -1
	if chosen.candidate.Candidate.Seeders != nil {
		reportedSeeders = *chosen.candidate.Candidate.Seeders
	}
	if strong {
		s.logger.Info("selected first strong ranked playback swarm", "id", chosen.session.ID,
			"name", chosen.candidate.Candidate.Name, "live_peers", chosen.peers,
			"reported_seeders", reportedSeeders)
	} else if chosen.peers >= minimumLivePeers {
		s.logger.Info("selected strongest live playback swarm", "id", chosen.session.ID,
			"name", chosen.candidate.Candidate.Name, "live_peers", chosen.peers,
			"reported_seeders", reportedSeeders)
	} else {
		s.logger.Warn("using best available playback despite weak live swarm", "id", chosen.session.ID,
			"name", chosen.candidate.Candidate.Name, "live_peers", chosen.peers,
			"reported_seeders", reportedSeeders)
	}
	return chosen.session, &chosen.candidate, nil
}

func (s *Server) dropPlaybackOptions(options []playbackOption) {
	for _, option := range options {
		_ = s.engine.Drop(option.session.ID)
	}
}

func publicRankedCandidate(candidate catalog.RankedCandidate) catalog.RankedCandidate {
	candidate.Candidate.MagnetURI = ""
	candidate.Candidate.TorrentURL = ""
	candidate.Candidate.NZBURL = ""
	return candidate
}

func (s *Server) playbackStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var selected *catalog.RankedCandidate
	s.mu.RLock()
	if value, ok := s.selected[id]; ok {
		copy := value
		selected = &copy
	}
	s.mu.RUnlock()

	if status, ok := s.engine.Status(id); ok {
		type response struct {
			torrentstream.Status
			Source   string                   `json:"source"`
			Selected *catalog.RankedCandidate `json:"selected,omitempty"`
		}
		writeJSON(w, http.StatusOK, response{Status: status, Source: catalog.ProtocolTorrent, Selected: selected})
		return
	}
	if s.usenetEngine != nil {
		if status, ok := s.usenetEngine.Status(id); ok {
			type response struct {
				usenetstream.Status
				Selected *catalog.RankedCandidate `json:"selected,omitempty"`
			}
			writeJSON(w, http.StatusOK, response{Status: status, Selected: selected})
			return
		}
	}
	writeError(w, http.StatusNotFound, "playback not found")
}

func (s *Server) streamPlayback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	if _, ok := s.engine.Get(id); ok {
		err = s.engine.ServeHTTP(w, r, id)
	} else if s.usenetEngine != nil {
		if _, ok := s.usenetEngine.Get(id); ok {
			err = s.usenetEngine.ServeHTTP(w, r, id)
		} else {
			err = errors.New("playback not found")
		}
	} else {
		err = errors.New("playback not found")
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, r.Context().Err()) {
			return
		}
		writeError(w, http.StatusNotFound, err.Error())
	}
}

func (s *Server) playbackExists(id string) bool {
	if s.engine != nil {
		if _, ok := s.engine.Get(id); ok {
			return true
		}
	}
	if s.usenetEngine != nil {
		_, ok := s.usenetEngine.Get(id)
		return ok
	}
	return false
}

func (s *Server) startHLSPlayback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.playbackExists(id) {
		writeError(w, http.StatusNotFound, "playback not found")
		return
	}
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		writeError(w, http.StatusNotImplemented, "HLS playback is not configured")
		return
	}
	defer r.Body.Close()
	var request struct {
		StartSeconds        float64 `json:"start_seconds,omitempty"`
		BitmapSubtitleIndex *int    `json:"bitmap_subtitle_index,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	s.mu.RLock()
	preferredLanguages := append([]string(nil), s.playbackLanguages[id]...)
	s.mu.RUnlock()
	bitmapSubtitleIndex := -1
	if request.BitmapSubtitleIndex != nil {
		if *request.BitmapSubtitleIndex < 0 {
			writeError(w, http.StatusBadRequest, "invalid bitmap subtitle index")
			return
		}
		bitmapSubtitleIndex = *request.BitmapSubtitleIndex
	}
	stream, err := manager.Start(
		r.Context(), id, request.StartSeconds, preferredLanguages, bitmapSubtitleIndex,
	)
	if err != nil {
		s.invalidateCachedPlayback(id)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.cacheSuccessfulPlayback(id)
	writeJSON(w, http.StatusCreated, struct {
		hls.Stream
		PlaylistURL string `json:"playlist_url"`
	}{
		Stream:      stream,
		PlaylistURL: fmt.Sprintf("%s://%s/v1/playbacks/%s/hls/index.m3u8", requestScheme(r), r.Host, id),
	})
}

func (s *Server) listHLSSubtitles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.playbackExists(id) {
		writeError(w, http.StatusNotFound, "playback not found")
		return
	}
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		writeError(w, http.StatusNotImplemented, "HLS playback is not configured")
		return
	}
	tracks, err := manager.ProbeSubtitles(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tracks)
}

func (s *Server) startHLSSubtitle(w http.ResponseWriter, r *http.Request) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		writeError(w, http.StatusNotImplemented, "HLS playback is not configured")
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		writeError(w, http.StatusBadRequest, "invalid subtitle index")
		return
	}
	if err := manager.StartSubtitle(r.Context(), r.PathValue("id"), index); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "subtitle track not found")
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "preparing"})
}

func (s *Server) parkHLSPlayback(w http.ResponseWriter, r *http.Request) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		writeError(w, http.StatusNotImplemented, "HLS playback is not configured")
		return
	}
	defer r.Body.Close()
	var request struct {
		BufferSeconds int `json:"buffer_seconds,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if request.BufferSeconds < 0 || request.BufferSeconds > 120 {
		writeError(w, http.StatusBadRequest, "buffer_seconds must be between 0 and 120")
		return
	}
	if err := manager.Park(r.Context(), r.PathValue("id"), request.BufferSeconds); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "HLS playback not found")
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "parked"})
}

func (s *Server) stopHLSPlayback(w http.ResponseWriter, r *http.Request) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	playbackID := r.PathValue("id")
	if manager != nil {
		manager.Stop(playbackID)
	}
	s.queuePlaybackHandoff(playbackID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) serveHLSAsset(w http.ResponseWriter, r *http.Request) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("asset")
	path, err := manager.AssetPath(r.PathValue("id"), name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case strings.HasSuffix(name, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		playlist, err := os.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		contents := string(playlist)
		if !strings.Contains(contents, "#EXT-X-START:") {
			contents = strings.Replace(
				contents,
				"#EXTM3U\n",
				"#EXTM3U\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n",
				1,
			)
		}
		_, _ = io.WriteString(w, contents)
		return
	case strings.HasSuffix(name, ".m4s"):
		w.Header().Set("Content-Type", "video/iso.segment")
	case strings.HasSuffix(name, ".mp4"):
		w.Header().Set("Content-Type", "video/mp4")
	case strings.HasSuffix(name, ".vtt"):
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	}
	http.ServeFile(w, r, path)
}

func playbackCacheMediaID(
	request CreatePlaybackRequest,
	source string,
	selected *catalog.RankedCandidate,
) string {
	if source == catalog.ProtocolTorrent && selected != nil {
		for _, reason := range selected.Reasons {
			if reason == "season pack" {
				if mediaID := seasonPlaybackCacheMediaID(request); mediaID != "" {
					return mediaID
				}
			}
		}
	}
	return request.MediaID
}

func seasonPlaybackCacheMediaID(request CreatePlaybackRequest) string {
	if request.SeriesID == "" || request.SeasonNumber <= 0 {
		return ""
	}
	return fmt.Sprintf("%s:s%d", request.SeriesID, request.SeasonNumber)
}

func playbackFileHint(request CreatePlaybackRequest) string {
	if request.SeasonNumber <= 0 || request.EpisodeNumber <= 0 {
		return ""
	}
	return fmt.Sprintf("S%02dE%02d", request.SeasonNumber, request.EpisodeNumber)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func mergeMovieRatings(primary, fallback metadata.MovieRatings) metadata.MovieRatings {
	if primary.IMDb == nil {
		primary.IMDb = fallback.IMDb
	}
	if primary.RottenTomatoes == nil {
		primary.RottenTomatoes = fallback.RottenTomatoes
	}
	if primary.ContentRating == nil {
		primary.ContentRating = fallback.ContentRating
	}
	return primary
}

func mergePreferences(defaults, requested catalog.Preferences) catalog.Preferences {
	result := requested
	if result.Resolution == "" {
		result.Resolution = defaults.Resolution
	}
	if len(result.Codecs) == 0 {
		result.Codecs = append([]string(nil), defaults.Codecs...)
	}
	if len(result.Languages) == 0 {
		result.Languages = append([]string(nil), defaults.Languages...)
	}
	if result.MaxSizeBytes == 0 {
		result.MaxSizeBytes = defaults.MaxSizeBytes
	}
	return result
}

func requestScheme(r *http.Request) string {
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		return forwarded
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
