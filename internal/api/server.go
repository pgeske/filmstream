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
	maxUsenetCandidates    = 30
	maxUsenetPreparation   = 90 * time.Second
	usenetFailureTTL       = 30 * time.Minute
	maxLiveSwarmCandidates = 3
	minimumLivePeers       = 3
	strongLiveSwarmPeers   = 20
	progressiveSwarmWait   = time.Second
	cachedSwarmWait        = 5 * time.Second
	liveSwarmWait          = 16 * time.Second
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
	Start(context.Context, string, float64, []string) (hls.Stream, error)
	StartSubtitle(context.Context, string, int) error
	AssetPath(string, string) (string, error)
	Prepared(string, float64, []string, int) bool
	Park(context.Context, string, int) error
	Stop(string)
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

type Server struct {
	indexers           *indexer.Registry
	engine             *torrentstream.Engine
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

	prewarmMu      sync.Mutex
	prewarmStates  map[string]*playbackPrewarmState
	prewarmSlots   chan struct{}
	prewarmTrigger chan struct{}
	prewarmBaseURL string
	prewarmClient  *http.Client
	prewarmContext context.Context
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
		prewarmTrigger:     make(chan struct{}, 1),
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
	mux.HandleFunc("POST /v1/playbacks", s.createPlayback)
	mux.HandleFunc("POST /v1/playbacks/prewarm", s.prewarmPlayback)
	mux.HandleFunc("GET /v1/playbacks/{id}", s.playbackStatus)
	mux.HandleFunc("GET /v1/playbacks/{id}/stream", s.streamPlayback)
	mux.HandleFunc("HEAD /v1/playbacks/{id}/stream", s.streamPlayback)
	mux.HandleFunc("POST /v1/playbacks/{id}/hls", s.startHLSPlayback)
	mux.HandleFunc("POST /v1/playbacks/{id}/hls/park", s.parkHLSPlayback)
	mux.HandleFunc("DELETE /v1/playbacks/{id}/hls", s.stopHLSPlayback)
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
		if resumable := firstResumableEpisode(seriesEntries); resumable != nil {
			result = append(result, *resumable)
			continue
		}
		if next, ok := s.nextUnwatchedEpisode(ctx, entry, seriesEntries); ok {
			result = append(result, next)
		}
	}
	return result
}

func firstResumableEpisode(entries []history.Entry) *history.Entry {
	for i := range entries {
		if entries[i].CanContinue() {
			return &entries[i]
		}
	}
	return nil
}

func (s *Server) nextUnwatchedEpisode(
	ctx context.Context,
	latest history.Entry,
	entries []history.Entry,
) (history.Entry, bool) {
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.ShowProvider)
	s.metadataMu.RUnlock()
	if !ok {
		return history.Entry{}, false
	}
	show, err := provider.Show(ctx, latest.SeriesID)
	if err != nil {
		return history.Entry{}, false
	}
	completed := make(map[string]bool)
	for _, entry := range entries {
		if entry.Completed {
			completed[entry.MediaID] = true
		}
	}
	for _, summary := range show.Seasons {
		season, err := provider.Season(ctx, show.ID, summary.Number)
		if err != nil {
			continue
		}
		for _, episode := range season.Episodes {
			if completed[episode.ID] {
				continue
			}
			return history.Entry{
				ID:              latest.ID,
				MediaID:         episode.ID,
				MediaType:       string(metadata.MediaTypeShow),
				Title:           show.Title,
				Year:            show.Year,
				Overview:        episode.Overview,
				PosterURL:       show.PosterURL,
				BackdropURL:     firstNonEmpty(episode.StillURL, show.BackdropURL),
				Genres:          show.Genres,
				NumberOfSeasons: show.NumberOfSeasons,
				SeriesID:        show.ID,
				SeriesTitle:     show.Title,
				SeasonNumber:    episode.SeasonNumber,
				EpisodeNumber:   episode.EpisodeNumber,
				EpisodeTitle:    episode.Title,
				UpdatedAt:       latest.UpdatedAt,
			}, true
		}
	}
	return history.Entry{}, false
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
	s.triggerPlaybackPrewarming()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) updateWatchProgress(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "watch history is not configured")
		return
	}
	defer r.Body.Close()
	var request struct {
		MediaID         string   `json:"media_id,omitempty"`
		MediaType       string   `json:"media_type,omitempty"`
		Title           string   `json:"title"`
		Year            int      `json:"year,omitempty"`
		Overview        string   `json:"overview,omitempty"`
		PosterURL       string   `json:"poster_url,omitempty"`
		BackdropURL     string   `json:"backdrop_url,omitempty"`
		Genres          []string `json:"genres,omitempty"`
		NumberOfSeasons int      `json:"number_of_seasons,omitempty"`
		SeriesID        string   `json:"series_id,omitempty"`
		SeriesTitle     string   `json:"series_title,omitempty"`
		SeasonNumber    int      `json:"season_number,omitempty"`
		EpisodeNumber   int      `json:"episode_number,omitempty"`
		EpisodeTitle    string   `json:"episode_title,omitempty"`
		PositionSeconds float64  `json:"position_seconds"`
		DurationSeconds float64  `json:"duration_seconds,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
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
		go s.queueNextEpisodePrewarm(entry)
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
		search := catalog.SearchRequest{
			Query: request.Query, Year: request.Year, MediaType: request.MediaType,
			SeasonNumber: request.SeasonNumber, EpisodeNumber: request.EpisodeNumber,
			Preferences: preferences,
		}
		allowUsenet := s.playbackSourceMode != config.PlaybackSourceTorrentOnly
		allowTorrent := s.playbackSourceMode != config.PlaybackSourceUsenetOnly
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
			session, selected, err = s.createCachedPlayback(r.Context(), request, preferences)
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
				ranked, searchErr = s.searchAndRank(r.Context(), search, request.OriginalTitle, "")
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
				mediaID: request.MediaID,
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
	session   *torrentstream.Session
	candidate catalog.RankedCandidate
	peers     int
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
		if ctx.Err() == nil {
			s.markUsenetCandidateFailed(cached.Selected.Candidate, playbackFileHint(request))
		}
		if removeErr := s.playbackCache.RemoveUsenet(request.MediaID, request.Query, request.Year); removeErr != nil {
			s.logger.Warn("remove unusable cached NZB", "title", request.Query, "error", removeErr)
		}
		s.logger.Warn("cached Usenet playback is unavailable; searching again",
			"title", request.Query, "name", cached.Selected.Candidate.Name, "error", err)
		return nil, nil
	}
	if preferences.PreferTextSubtitles {
		hasTextSubtitles, probeErr := s.playbackHasTextSubtitles(preparationContext, session.ID)
		if probeErr != nil {
			_ = s.usenetEngine.Drop(session.ID)
			s.markUsenetCandidateFailed(cached.Selected.Candidate, playbackFileHint(request))
			if removeErr := s.playbackCache.RemoveUsenet(request.MediaID, request.Query, request.Year); removeErr != nil {
				s.logger.Warn("remove cached NZB that failed media probing", "title", request.Query, "error", removeErr)
			}
			s.logger.Warn("cached Usenet playback failed media probing; searching again",
				"title", request.Query, "name", cached.Selected.Candidate.Name, "error", probeErr)
			return nil, nil
		}
		if !hasTextSubtitles {
			_ = s.usenetEngine.Drop(session.ID)
			if removeErr := s.playbackCache.RemoveUsenet(request.MediaID, request.Query, request.Year); removeErr != nil {
				s.logger.Warn("remove cached NZB without text subtitles", "title", request.Query, "error", removeErr)
			}
			s.logger.Info("cached Usenet playback lacks text subtitles; searching again", "title", request.Query)
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
	preferences catalog.Preferences,
) (*playbackSession, *catalog.RankedCandidate, error) {
	if s.playbackCache == nil {
		return nil, nil, nil
	}
	cached, found, err := s.playbackCache.Lookup(request.MediaID, request.Query, request.Year)
	if err != nil {
		s.logger.Warn("load cached playback selection", "title", request.Query, "error", err)
		return nil, nil, nil
	}
	if !found {
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
		if removeErr := s.playbackCache.Remove(request.MediaID, request.Query, request.Year); removeErr != nil {
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
		if removeErr := s.playbackCache.Remove(request.MediaID, request.Query, request.Year); removeErr != nil {
			s.logger.Warn("remove unavailable playback cache", "title", request.Query, "error", removeErr)
		}
		s.logger.Warn("cached playback swarm is unavailable; searching again",
			"title", request.Query, "live_peers", options[0].peers)
		return nil, nil, nil
	}
	if preferences.PreferTextSubtitles {
		hasTextSubtitles, probeErr := s.playbackHasTextSubtitles(ctx, session.ID)
		if probeErr != nil {
			s.logger.Warn("probe cached playback subtitles", "title", request.Query, "error", probeErr)
		} else if !hasTextSubtitles {
			_ = s.engine.Drop(session.ID)
			if removeErr := s.playbackCache.Remove(request.MediaID, request.Query, request.Year); removeErr != nil {
				s.logger.Warn("remove cached playback without text subtitles", "title", request.Query, "error", removeErr)
			}
			s.logger.Info("cached playback lacks text subtitles; searching again", "title", request.Query)
			return nil, nil, nil
		}
	}
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
				hasTextSubtitles, probeErr := s.playbackHasTextSubtitles(preparationContext, session.ID)
				if probeErr != nil {
					_ = s.usenetEngine.Drop(session.ID)
					s.markUsenetCandidateFailed(candidate.Candidate, fileHint)
					failures = append(failures, fmt.Sprintf("%s: probe media: %v", candidate.Candidate.Name, probeErr))
					s.logger.Warn("reject Usenet playback that failed media probing", "id", session.ID,
						"name", candidate.Candidate.Name, "error", probeErr)
					continue
				}
				if hasTextSubtitles {
					if fallbackSession != nil {
						_ = s.usenetEngine.Drop(fallbackSession.ID)
					}
					s.logger.Info("selected Usenet playback with text subtitles",
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
		s.markUsenetCandidateFailed(candidate.Candidate, fileHint)
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.Candidate.Name, err))
	}
	if fallbackSession != nil {
		s.logger.Warn("no Usenet candidate exposed text subtitles; using best available release",
			"id", fallbackSession.ID, "name", fallbackCandidate.Candidate.Name)
		return wrapUsenetSession(fallbackSession), fallbackCandidate, nil
	}
	if len(failures) > 0 {
		return nil, nil, errors.New(strings.Join(failures, "; "))
	}
	return nil, nil, nil
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
	options := make([]playbackOption, 0, attempts)
	observationRemaining := liveSwarmWait
	var failures []string
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
		options = append(options, playbackOption{session: session, candidate: candidate})

		if i+1 < attempts && observationRemaining > 0 {
			wait := min(progressiveSwarmWait, observationRemaining)
			started := time.Now()
			strong, err := s.observePlaybackOptions(ctx, options, strongLiveSwarmPeers, wait)
			observationRemaining -= min(time.Since(started), observationRemaining)
			if err != nil {
				s.dropPlaybackOptions(options)
				return nil, nil, err
			}
			if strong >= 0 && !preferences.PreferTextSubtitles {
				return s.choosePlaybackOption(options, strong, true)
			}
		}
	}
	if len(options) == 0 {
		if len(failures) > 0 {
			return nil, nil, errors.New(strings.Join(failures, "; "))
		}
		return nil, nil, errors.New("no usable torrent candidates found")
	}

	strong, err := s.observePlaybackOptions(ctx, options, strongLiveSwarmPeers, observationRemaining)
	if err != nil {
		s.dropPlaybackOptions(options)
		return nil, nil, err
	}
	if preferences.PreferTextSubtitles {
		if subtitleOption := s.firstTextSubtitleOption(ctx, options); subtitleOption >= 0 {
			return s.choosePlaybackOption(
				options,
				subtitleOption,
				options[subtitleOption].peers >= strongLiveSwarmPeers,
			)
		}
		s.logger.Warn("no live ranked playback option exposed text subtitles; using best available release")
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
	return s.choosePlaybackOption(options, best, false)
}

func (s *Server) playbackHasTextSubtitles(ctx context.Context, playbackID string) (bool, error) {
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

func (s *Server) firstTextSubtitleOption(ctx context.Context, options []playbackOption) int {
	for index, option := range options {
		if option.peers < minimumLivePeers {
			continue
		}
		hasTextSubtitles, err := s.playbackHasTextSubtitles(ctx, option.session.ID)
		if err != nil {
			s.logger.Warn("probe playback subtitles", "id", option.session.ID,
				"name", option.candidate.Candidate.Name, "error", err)
			continue
		}
		if hasTextSubtitles {
			s.logger.Info("preferred playback with text subtitles", "id", option.session.ID,
				"name", option.candidate.Candidate.Name, "live_peers", option.peers)
			return index
		}
	}
	return -1
}

func (s *Server) observePlaybackOptions(
	ctx context.Context,
	options []playbackOption,
	threshold int,
	wait time.Duration,
) (int, error) {
	refresh := func() int {
		for i := range options {
			status, ok := s.engine.Status(options[i].session.ID)
			if ok {
				options[i].peers = max(status.ActivePeers, status.ConnectedSeeders)
			}
		}
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
	ticker := time.NewTicker(250 * time.Millisecond)
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
	if _, ok := s.engine.Get(id); ok {
		return true
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
		StartSeconds float64 `json:"start_seconds,omitempty"`
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
	stream, err := manager.Start(r.Context(), id, request.StartSeconds, preferredLanguages)
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
	if !s.queuePlaybackHandoff(playbackID) {
		s.triggerPlaybackPrewarming()
	}
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
	case strings.HasSuffix(name, ".m4s"):
		w.Header().Set("Content-Type", "video/iso.segment")
	case strings.HasSuffix(name, ".mp4"):
		w.Header().Set("Content-Type", "video/mp4")
	case strings.HasSuffix(name, ".vtt"):
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	}
	http.ServeFile(w, r, path)
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
