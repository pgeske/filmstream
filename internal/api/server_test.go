package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
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

type fakeResolver struct{}

type fakeMetadata struct{}

type fakeRatings struct{}

type partialRatings struct{}

type fakeUsenetPlaybackEngine struct {
	createdSource usenetstream.Source
	created       []usenetstream.Source
	createErr     error
	createFunc    func(usenetstream.Source) (*usenetstream.Session, error)
	session       *usenetstream.Session
	nzb           []byte
	cleanup       func(string, string)
}

func (f *fakeUsenetPlaybackEngine) SetCleanupHandler(handler func(string, string)) {
	f.cleanup = handler
}

func (f *fakeUsenetPlaybackEngine) Create(_ context.Context, source usenetstream.Source) (*usenetstream.Session, error) {
	f.createdSource = source
	f.created = append(f.created, source)
	if f.createFunc != nil {
		return f.createFunc(source)
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.session, nil
}

func (f *fakeUsenetPlaybackEngine) Get(id string) (*usenetstream.Session, bool) {
	return f.session, f.session != nil && f.session.ID == id
}

func (f *fakeUsenetPlaybackEngine) NZB(id string) ([]byte, error) {
	if f.session == nil || f.session.ID != id {
		return nil, errors.New("playback not found")
	}
	return append([]byte(nil), f.nzb...), nil
}

func (f *fakeUsenetPlaybackEngine) Status(id string) (usenetstream.Status, bool) {
	if f.session == nil || f.session.ID != id {
		return usenetstream.Status{}, false
	}
	return usenetstream.Status{ID: id, Source: catalog.ProtocolUsenet}, true
}

func (*fakeUsenetPlaybackEngine) ServeHTTP(http.ResponseWriter, *http.Request, string) error {
	return nil
}

func (*fakeUsenetPlaybackEngine) Drop(string) error { return nil }

type fakeHLSManager struct {
	dir              string
	parked           string
	stopped          string
	subtitlePlayback string
	subtitleIndex    int
	subtitleTracks   map[string][]hls.SubtitleTrack
	probeErrors      map[string]error
	languages        []string
}

func (f *fakeHLSManager) ProbeSubtitles(_ context.Context, id string) ([]hls.SubtitleTrack, error) {
	if err := f.probeErrors[id]; err != nil {
		return nil, err
	}
	return f.subtitleTracks[id], nil
}

func (f *fakeHLSManager) Start(_ context.Context, id string, start float64, languages []string) (hls.Stream, error) {
	f.languages = append([]string(nil), languages...)
	return hls.Stream{PlaybackID: id, StartSeconds: start, VideoCodec: "h264"}, nil
}

func (f *fakeHLSManager) StartSubtitle(_ context.Context, playbackID string, index int) error {
	f.subtitlePlayback = playbackID
	f.subtitleIndex = index
	return nil
}

func (f *fakeHLSManager) AssetPath(_, name string) (string, error) {
	return filepath.Join(f.dir, name), nil
}

func (f *fakeHLSManager) Prepared(string, float64, []string, int) bool {
	return false
}

func (f *fakeHLSManager) Park(_ context.Context, id string, _ int) error {
	f.parked = id
	return nil
}

func (f *fakeHLSManager) Stop(id string) {
	f.stopped = id
}

func (fakeMetadata) IMDbID(_ context.Context, mediaID string) (string, error) {
	if mediaID == "tmdb:1" {
		return "tt1234567", nil
	}
	return "", errors.New("movie has no IMDb ID")
}

func (fakeMetadata) Search(_ context.Context, _ string) ([]metadata.Movie, error) {
	return []metadata.Movie{{
		ID:        "tmdb:1",
		Title:     "The Movie",
		Year:      2001,
		PosterURL: "https://image.example/poster.jpg",
		Genres:    []string{"Drama", "Thriller"},
	}}, nil
}

func (fakeRatings) RatingsByIMDbID(_ context.Context, imdbID string) (metadata.MovieRatings, error) {
	if imdbID != "tt1234567" {
		return metadata.MovieRatings{}, errors.New("unexpected IMDb ID")
	}
	imdb := 9.1
	rottenTomatoes := 95
	contentRating := "PG-13"
	return metadata.MovieRatings{IMDb: &imdb, RottenTomatoes: &rottenTomatoes, ContentRating: &contentRating}, nil
}

func (fakeRatings) Ratings(_ context.Context, title string, year int) (metadata.MovieRatings, error) {
	if title != "The Movie" || year != 2001 {
		return metadata.MovieRatings{}, nil
	}
	imdb := 8.4
	rottenTomatoes := 91
	contentRating := "PG"
	return metadata.MovieRatings{IMDb: &imdb, RottenTomatoes: &rottenTomatoes, ContentRating: &contentRating}, nil
}

func (partialRatings) RatingsByIMDbID(_ context.Context, _ string) (metadata.MovieRatings, error) {
	imdb := 8.4
	return metadata.MovieRatings{IMDb: &imdb}, nil
}

func (partialRatings) Ratings(_ context.Context, _ string, _ int) (metadata.MovieRatings, error) {
	rottenTomatoes := 91
	return metadata.MovieRatings{RottenTomatoes: &rottenTomatoes}, nil
}

func (fakeMetadata) Discover(_ context.Context, collection metadata.Collection) ([]metadata.Movie, error) {
	switch collection {
	case metadata.CollectionPopular:
		return []metadata.Movie{{ID: "tmdb:2", MediaType: metadata.MediaTypeMovie, Title: "Popular Movie", Year: 2026}}, nil
	case metadata.CollectionTopRated:
		return []metadata.Movie{{ID: "tmdb-tv:3", MediaType: metadata.MediaTypeShow, Title: "Top Rated Show", Year: 2020, NumberOfSeasons: 3}}, nil
	default:
		return nil, nil
	}
}

func (fakeMetadata) Show(_ context.Context, id string) (metadata.Show, error) {
	if id != "tmdb-tv:3" {
		return metadata.Show{}, errors.New("show not found")
	}
	return metadata.Show{
		Movie: metadata.Movie{
			ID: id, MediaType: metadata.MediaTypeShow, Title: "Top Rated Show",
			OriginalLanguage: "ja", Year: 2020, NumberOfSeasons: 3,
		},
		Seasons: []metadata.SeasonSummary{{Number: 1, Name: "Season 1", EpisodeCount: 8}},
	}, nil
}

func (fakeMetadata) Season(_ context.Context, id string, number int) (metadata.Season, error) {
	if id != "tmdb-tv:3" || number != 1 {
		return metadata.Season{}, errors.New("season not found")
	}
	return metadata.Season{
		SeriesID: id, SeriesTitle: "Top Rated Show", Number: number, Name: "Season 1",
		Episodes: []metadata.Episode{
			{
				ID: "tmdb-tv:3:s1:e1", SeriesID: id, SeriesTitle: "Top Rated Show",
				SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot",
			},
			{
				ID: "tmdb-tv:3:s1:e2", SeriesID: id, SeriesTitle: "Top Rated Show",
				SeasonNumber: 1, EpisodeNumber: 2, Title: "Second",
			},
		},
	}, nil
}

func (fakeResolver) Resolve(_ context.Context, input string) (resolver.Result, error) {
	return resolver.Result{Input: input, Candidates: []resolver.Candidate{{Title: "The Movie", Year: 2001, Confidence: 0.9}}}, nil
}

func TestCreatePlaybackPrefersUsenetCandidate(t *testing.T) {
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q,year"/></searching></caps>`)
		case "movie":
			fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Sintel.2010.1080p.WEB.H264-GROUP</title><guid>release-1</guid><enclosure url="/download/1" length="607836000" type="application/x-nzb"/></item></channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "usenet", Type: "torznab", Endpoint: indexerServer.URL + "/6/api", APIKey: "secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: t.TempDir(), MetadataTimeout: time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{
		Codecs: []string{"h264", "h265"}, MaxSizeBytes: 60 << 30,
	}, slog.Default())
	fakeUsenet := &fakeUsenetPlaybackEngine{session: &usenetstream.Session{
		ID: "usenet-playback", Name: "Sintel", FileName: "Sintel.mkv", FileSize: 607836000,
	}}
	server.usenetEngine = fakeUsenet
	server.hlsManager = &fakeHLSManager{subtitleTracks: map[string][]hls.SubtitleTrack{
		"usenet-playback": {{Index: 2, Language: "en"}},
	}}

	request := httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(
		`{"query":"Sintel","year":2010,"preferences":{"streaming_optimized":true,"prefer_text_subtitles":true,"codecs":["h264","h265"],"languages":["en","english"]}}`,
	))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload CreatePlaybackResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != catalog.ProtocolUsenet || payload.ID != "usenet-playback" {
		t.Fatalf("payload = %+v", payload)
	}
	if fakeUsenet.createdSource.NZBURL != indexerServer.URL+"/download/1" {
		t.Fatalf("source = %+v", fakeUsenet.createdSource)
	}
	if payload.Selected == nil || payload.Selected.Candidate.Protocol != catalog.ProtocolUsenet || payload.Selected.Candidate.NZBURL != "" {
		t.Fatalf("selected = %+v", payload.Selected)
	}
	if got := server.playbackLanguages[payload.ID]; !slices.Equal(got, []string{"en", "english"}) {
		t.Fatalf("playback languages = %v", got)
	}
}

func TestCreateEpisodePlaybackUsesTVSearchAndFileHint(t *testing.T) {
	var tvSearchQuery string
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`)
		case "tvsearch":
			tvSearchQuery = r.URL.RawQuery
			if r.URL.Query().Get("q") == "Original Show" {
				fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Original.Show.S01E02.1080p.WEB.H264-GROUP</title><guid>episode-2</guid><enclosure url="/download/episode-2" length="607836000" type="application/x-nzb"/></item></channel></rss>`)
			} else {
				fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel/></rss>`)
			}
		case "search":
			fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel/></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "usenet", Type: "torznab", Endpoint: indexerServer.URL + "/api", APIKey: "secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: t.TempDir(), MetadataTimeout: time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{Codecs: []string{"h264"}}, slog.Default())
	fakeUsenet := &fakeUsenetPlaybackEngine{session: &usenetstream.Session{
		ID: "episode-playback", Name: "Top Rated Show", FileName: "Top.Rated.Show.S01E02.mkv",
	}}
	server.usenetEngine = fakeUsenet

	body := `{"media_id":"tmdb-tv:3:s1:e2","media_type":"show","query":"Localized Name","original_title":"Original Show","year":2020,"series_id":"tmdb-tv:3","series_title":"Localized Name","season_number":1,"episode_number":2,"episode_title":"Second","preferences":{"codecs":["h264"]}}`
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(tvSearchQuery, "season=1") || !strings.Contains(tvSearchQuery, "ep=2") || !strings.Contains(tvSearchQuery, "q=Original+Show") {
		t.Fatalf("TV search query = %q", tvSearchQuery)
	}
	if fakeUsenet.createdSource.FileHint != "S01E02" {
		t.Fatalf("source = %+v", fakeUsenet.createdSource)
	}
}

func TestCreatePlaybackReusesCachedUsenetReleaseWithoutSearching(t *testing.T) {
	var searches atomic.Int32
	var indexerServer *httptest.Server
	indexerServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q,year"/></searching></caps>`)
		case "movie", "search":
			searches.Add(1)
			fmt.Fprintf(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Sintel.2010.1080p.WEB.H264-GROUP</title><guid>release-1</guid><enclosure url="%s/download/1" length="607836000" type="application/x-nzb"/></item></channel></rss>`, indexerServer.URL)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "usenet", Type: "torznab", Endpoint: indexerServer.URL + "/api", APIKey: "secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: t.TempDir(), MetadataTimeout: time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{}, slog.Default())
	fakeUsenet := &fakeUsenetPlaybackEngine{
		session: &usenetstream.Session{ID: "usenet-playback", Name: "Sintel", FileName: "Sintel.mkv", FileSize: 607836000},
		nzb:     []byte(`<?xml version="1.0"?><nzb/>`),
	}
	server.usenetEngine = fakeUsenet
	server.playbackSourceMode = config.PlaybackSourceUsenetOnly
	server.hlsManager = &fakeHLSManager{}
	store := playbackcache.New(t.TempDir())
	server.SetPlaybackCache(store)

	create := func() CreatePlaybackResponse {
		request := httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(
			`{"media_id":"tmdb:1","query":"Sintel","year":2010}`,
		))
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var payload CreatePlaybackResponse
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	first := create()
	searchesAfterFirst := searches.Load()
	if searchesAfterFirst == 0 || fakeUsenet.createdSource.NZBURL == "" {
		t.Fatalf("initial searches = %d, source = %+v", searchesAfterFirst, fakeUsenet.createdSource)
	}
	hlsRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/playbacks/"+first.ID+"/hls",
		strings.NewReader(`{}`),
	)
	hlsResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(hlsResponse, hlsRequest)
	if hlsResponse.Code != http.StatusCreated {
		t.Fatalf("HLS status = %d, body = %s", hlsResponse.Code, hlsResponse.Body.String())
	}
	if _, found, err := store.LookupUsenet("tmdb:1", "Sintel", 2010); err != nil || !found {
		t.Fatalf("cached Usenet playback found = %v, error = %v", found, err)
	}

	create()
	if searches.Load() != searchesAfterFirst {
		t.Fatalf("search count after cached playback = %d, want %d", searches.Load(), searchesAfterFirst)
	}
	if fakeUsenet.createdSource.NZBPath == "" || fakeUsenet.createdSource.NZBURL != "" {
		t.Fatalf("cached source = %+v", fakeUsenet.createdSource)
	}
}

func TestParkHLSPlayback(t *testing.T) {
	manager := &fakeHLSManager{}
	server := &Server{hlsManager: manager}
	request := httptest.NewRequest(http.MethodPost, "/v1/playbacks/warm-playback/hls/park", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if manager.parked != "warm-playback" {
		t.Fatalf("parked playback = %q", manager.parked)
	}
}

func TestCreatePlaybackJoinsInProgressPrewarm(t *testing.T) {
	var createRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/playbacks":
			createRequests.Add(1)
			if r.Header.Get(prewarmRequestHeader) == "" {
				t.Error("internal prewarm header is missing")
			}
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"warm-playback","name":"The Movie","file_name":"movie.mkv","file_size":1000,"source":"usenet","stream_url":"http://127.0.0.1/stream"}`)
		case "/v1/playbacks/warm-playback/hls":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case "/v1/playbacks/warm-playback/hls/park":
			fmt.Fprint(w, `{"status":"parked"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	registry, err := indexer.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: t.TempDir(), MetadataTimeout: time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{}, slog.Default())
	server.usenetEngine = &fakeUsenetPlaybackEngine{
		session: &usenetstream.Session{ID: "warm-playback", Name: "The Movie", FileName: "movie.mkv"},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx
	server.prewarmBaseURL = upstream.URL
	server.prewarmClient = upstream.Client()
	server.queuePlaybackPrewarm(playbackPrewarmTarget{
		request: CreatePlaybackRequest{MediaID: "tmdb:1", Query: "The Movie", Year: 2001, StartSeconds: 600},
		source:  "hint", priority: true,
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(
		`{"media_id":"tmdb:1","query":"The Movie","year":2001,"start_seconds":600}`,
	))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"id":"warm-playback"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if createRequests.Load() != 1 {
		t.Fatalf("create requests = %d, want 1", createRequests.Load())
	}
}

func TestClaimPrewarmedPlaybackCancelsPendingPark(t *testing.T) {
	registry, err := indexer.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: t.TempDir(), MetadataTimeout: time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{}, slog.Default())
	server.usenetEngine = &fakeUsenetPlaybackEngine{
		session: &usenetstream.Session{ID: "warm-playback", Name: "The Movie", FileName: "movie.mkv"},
	}
	ready := make(chan struct{})
	close(ready)
	var parkCanceled atomic.Bool
	request := CreatePlaybackRequest{MediaID: "tmdb:1", Query: "The Movie", StartSeconds: 600}
	server.prewarmStates["tmdb:1"] = &playbackPrewarmState{
		target: playbackPrewarmTarget{request: request}, playbackReady: ready,
		response: CreatePlaybackResponse{ID: "warm-playback"},
		cancel:   func() {}, parkCancel: func() { parkCanceled.Store(true) },
	}

	response, ok := server.claimPrewarmedPlayback(t.Context(), request)
	if !ok || response.ID != "warm-playback" || !parkCanceled.Load() {
		t.Fatalf("response = %+v, claimed = %v, park canceled = %v", response, ok, parkCanceled.Load())
	}
}

func TestHistoryPrewarmPrefersOriginalShowAudio(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	target := server.prewarmTargetForHistory(t.Context(), history.Entry{
		MediaID: "tmdb-tv:3:s1:e1", MediaType: "show", Title: "Top Rated Show",
		SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show", SeasonNumber: 1, EpisodeNumber: 1,
	})
	if !slices.Equal(target.request.Preferences.Languages, []string{"ja", "en", "english"}) {
		t.Fatalf("languages = %v", target.request.Preferences.Languages)
	}
}

func TestNextEpisodeIsIncludedInPrewarming(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	next, ok := server.nextEpisodeForPrewarming(t.Context(), history.Entry{
		MediaID: "tmdb-tv:3:s1:e1", MediaType: "show", Title: "Top Rated Show",
		SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show", SeasonNumber: 1, EpisodeNumber: 1,
	})
	if !ok || next.MediaID != "tmdb-tv:3:s1:e2" || next.SeasonNumber != 1 || next.EpisodeNumber != 2 {
		t.Fatalf("next episode = %+v, found = %v", next, ok)
	}
}

func TestRankedUsenetPlaybackTriesPastTenIncompletePosts(t *testing.T) {
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "usenet", Type: "torznab", Endpoint: "https://indexer.example/api",
	}})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	fakeUsenet := &fakeUsenetPlaybackEngine{
		createFunc: func(source usenetstream.Source) (*usenetstream.Session, error) {
			attempts++
			if attempts <= 10 {
				return nil, errors.New("articles unavailable")
			}
			return &usenetstream.Session{
				ID: "healthy-release", Name: source.Name, FileName: "episode.mkv",
			}, nil
		},
	}
	server := &Server{
		indexers:       registry,
		usenetEngine:   fakeUsenet,
		usenetFailures: make(map[string]time.Time),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	var ranked []catalog.RankedCandidate
	for i := 1; i <= 11; i++ {
		ranked = append(ranked, catalog.RankedCandidate{Candidate: catalog.Candidate{
			ID: fmt.Sprintf("release-%d", i), Indexer: "usenet",
			Name: fmt.Sprintf("Show.S01E01.Release.%d", i), Protocol: catalog.ProtocolUsenet,
			NZBURL: fmt.Sprintf("https://indexer.example/download/%d", i),
		}})
	}

	session, selected, err := server.createRankedUsenetPlayback(
		t.Context(), ranked, catalog.Preferences{}, "S01E01",
	)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || selected == nil || selected.Candidate.ID != "release-11" {
		t.Fatalf("session = %+v, selected = %+v", session, selected)
	}
	if attempts != 11 {
		t.Fatalf("attempts = %d, want 11", attempts)
	}
}

func TestRankedUsenetPlaybackRejectsMediaProbeFailure(t *testing.T) {
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "usenet", Type: "torznab", Endpoint: "https://indexer.example/api",
	}})
	if err != nil {
		t.Fatal(err)
	}
	attempt := 0
	fakeUsenet := &fakeUsenetPlaybackEngine{createFunc: func(source usenetstream.Source) (*usenetstream.Session, error) {
		attempt++
		return &usenetstream.Session{
			ID: fmt.Sprintf("playback-%d", attempt), Name: source.Name, FileName: "episode.mkv",
		}, nil
	}}
	server := &Server{
		indexers: registry, usenetEngine: fakeUsenet,
		hlsManager: &fakeHLSManager{
			probeErrors: map[string]error{"playback-1": errors.New("probe playback media: exit status 1")},
			subtitleTracks: map[string][]hls.SubtitleTrack{
				"playback-2": {{Index: 2, Language: "en"}},
			},
		},
		usenetFailures: make(map[string]time.Time),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ranked := []catalog.RankedCandidate{
		{Candidate: catalog.Candidate{
			ID: "broken", Indexer: "usenet", Name: "Show.S01E01.Broken",
			Protocol: catalog.ProtocolUsenet, NZBURL: "https://indexer.example/broken",
		}},
		{Candidate: catalog.Candidate{
			ID: "healthy", Indexer: "usenet", Name: "Show.S01E01.Healthy",
			Protocol: catalog.ProtocolUsenet, NZBURL: "https://indexer.example/healthy",
		}},
	}

	session, selected, err := server.createRankedUsenetPlayback(
		t.Context(), ranked, catalog.Preferences{PreferTextSubtitles: true}, "S01E01",
	)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.ID != "playback-2" || selected == nil || selected.Candidate.ID != "healthy" {
		t.Fatalf("session = %+v, selected = %+v", session, selected)
	}
	if !server.usenetCandidateRecentlyFailed(ranked[0].Candidate, "S01E01") {
		t.Fatal("candidate that failed media probing was not skipped")
	}
}

func TestUnavailableCachedUsenetReleaseIsRemoved(t *testing.T) {
	store := playbackcache.New(t.TempDir())
	candidate := catalog.RankedCandidate{Candidate: catalog.Candidate{
		ID: "release-1", Indexer: "usenet", Name: "Movie.1080p", Protocol: catalog.ProtocolUsenet,
	}}
	if _, err := store.SaveUsenet("tmdb:1", "Movie", 2001, candidate, []byte("nzb")); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		usenetEngine:   &fakeUsenetPlaybackEngine{createErr: errors.New("articles unavailable")},
		playbackCache:  store,
		usenetFailures: make(map[string]time.Time),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	session, selected := server.createCachedUsenetPlayback(t.Context(), CreatePlaybackRequest{
		MediaID: "tmdb:1", Query: "Movie", Year: 2001,
	}, catalog.Preferences{})
	if session != nil || selected != nil {
		t.Fatalf("session = %+v, selected = %+v", session, selected)
	}
	if _, found, err := store.LookupUsenet("tmdb:1", "Movie", 2001); err != nil || found {
		t.Fatalf("cached Usenet playback found = %v, error = %v", found, err)
	}
	if !server.usenetCandidateRecentlyFailed(candidate.Candidate) {
		t.Fatal("unavailable cached candidate was not temporarily skipped")
	}
}

func TestHLSFailureInvalidatesAndSkipsUsenetRelease(t *testing.T) {
	store := playbackcache.New(t.TempDir())
	candidate := catalog.RankedCandidate{Candidate: catalog.Candidate{
		ID: "broken-release", Indexer: "usenet", Name: "Show.S09E03.1080p",
		Protocol: catalog.ProtocolUsenet,
	}}
	if _, err := store.SaveUsenet(
		"tmdb-tv:1:s9:e3", "Show", 2005, candidate, []byte("nzb"),
	); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		playbackCache: store,
		playbackCacheKeys: map[string]playbackCacheKey{
			"broken-playback": {
				mediaID: "tmdb-tv:1:s9:e3", title: "Show", year: 2005,
				source: catalog.ProtocolUsenet,
			},
		},
		selected: map[string]catalog.RankedCandidate{"broken-playback": candidate},
		playbackRequests: map[string]CreatePlaybackRequest{
			"broken-playback": {SeasonNumber: 9, EpisodeNumber: 3},
		},
		usenetFailures: make(map[string]time.Time),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	server.invalidateCachedPlayback("broken-playback")

	if _, found, err := store.LookupUsenet("tmdb-tv:1:s9:e3", "Show", 2005); err != nil || found {
		t.Fatalf("cached Usenet playback found = %v, error = %v", found, err)
	}
	if !server.usenetCandidateRecentlyFailed(candidate.Candidate, "S09E03") {
		t.Fatal("release that failed HLS packaging was not temporarily skipped")
	}
}

func TestCreatePlaybackFallsBackToTorrent(t *testing.T) {
	dataDir := t.TempDir()
	torrentContents := createAPITestTorrent(t, dataDir)
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/2" {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(torrentContents)
			return
		}
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q,year"/></searching></caps>`)
		case "movie":
			fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
				<item><title>Sintel.2010.1080p.WEB.H264-USENET</title><guid>release-1</guid><enclosure url="/download/1" length="607836000" type="application/x-nzb"/></item>
				<item><title>Sintel.2010.1080p.WEB.H264-TORRENT</title><guid>release-2</guid><enclosure url="/download/2" length="43008" type="application/x-bittorrent"/></item>
			</channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "mixed", Type: "torznab", Endpoint: indexerServer.URL + "/api",
	}})
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: dataDir, MetadataTimeout: 5 * time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{MaxSizeBytes: 1 << 30}, slog.Default())
	server.usenetEngine = &fakeUsenetPlaybackEngine{createErr: errors.New("articles unavailable")}

	request := httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(
		`{"query":"Sintel","year":2010}`,
	))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload CreatePlaybackResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != catalog.ProtocolTorrent || payload.Selected == nil || payload.Selected.Candidate.Protocol != catalog.ProtocolTorrent {
		t.Fatalf("payload = %+v", payload)
	}

	server.SetPlaybackSourceMode(config.PlaybackSourceUsenetOnly)
	request = httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(
		`{"query":"Sintel","year":2010}`,
	))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway ||
		!strings.Contains(response.Body.String(), "Usenet playback unavailable") ||
		strings.Contains(response.Body.String(), "articles unavailable") {
		t.Fatalf("Usenet-only status = %d, body = %s", response.Code, response.Body.String())
	}
}

func createAPITestTorrent(t *testing.T, dataDir string) []byte {
	t.Helper()
	torrentDataDir := filepath.Join(dataDir, "torrents")
	if err := os.MkdirAll(torrentDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	videoPath := filepath.Join(torrentDataDir, "Sintel.mp4")
	if err := os.WriteFile(videoPath, bytes.Repeat([]byte("filmstream-test"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := metainfo.MetaInfo{}
	meta.SetDefaults()
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(videoPath); err != nil {
		t.Fatal(err)
	}
	var err error
	meta.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := meta.Write(&output); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestResolveMovie(t *testing.T) {
	server := &Server{movieResolver: fakeResolver{}}
	request := httptest.NewRequest(http.MethodPost, "/v1/resolve", strings.NewReader(`{"query":"rough description"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "The Movie") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSearchCatalogReturnsMetadata(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/search?query=The+Movie", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "poster.jpg") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCatalogRatingsReturnsExternalScores(t *testing.T) {
	server := &Server{ratingsProvider: fakeRatings{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/ratings?title=The+Movie&year=2001", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"imdb":8.4`) || !strings.Contains(response.Body.String(), `"rotten_tomatoes":91`) || !strings.Contains(response.Body.String(), `"content_rating":"PG"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCatalogRatingsUsesTMDBMovieIDForReliableLookup(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}, ratingsProvider: fakeRatings{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/ratings?title=Localized+Movie&year=2001&media_id=tmdb%3A1", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"imdb":9.1`) || !strings.Contains(response.Body.String(), `"rotten_tomatoes":95`) || !strings.Contains(response.Body.String(), `"content_rating":"PG-13"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCatalogRatingsMergesIMDbAndTitleFallbacks(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}, ratingsProvider: partialRatings{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/ratings?title=The+Movie&year=2001&media_id=tmdb%3A1", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"imdb":8.4`) || !strings.Contains(response.Body.String(), `"rotten_tomatoes":91`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCatalogRatingsRequiresProvider(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/ratings?title=The+Movie&year=2001", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestDiscoverCatalogReturnsMixedMediaSections(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/discover", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Sections []catalogSection `json:"sections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sections) != 2 {
		t.Fatalf("sections = %+v", payload.Sections)
	}
	if payload.Sections[0].ID != "popular" || payload.Sections[0].Items[0].Title != "Popular Movie" {
		t.Fatalf("popular = %+v", payload.Sections[0])
	}
	if payload.Sections[1].ID != "top-rated" || payload.Sections[1].Items[0].Title != "Top Rated Show" || payload.Sections[1].Items[0].MediaType != metadata.MediaTypeShow {
		t.Fatalf("top rated = %+v", payload.Sections[1])
	}
}

func TestShowCatalogReturnsSeasonsAndEpisodes(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/catalog/shows/tmdb-tv:3", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"number_of_seasons":3`) || !strings.Contains(response.Body.String(), `"episode_count":8`) {
		t.Fatalf("show status = %d, body = %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/catalog/shows/tmdb-tv:3/seasons/1", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"tmdb-tv:3:s1:e1"`) || !strings.Contains(response.Body.String(), `"title":"Pilot"`) {
		t.Fatalf("season status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestWatchHistoryProgressRoundTrip(t *testing.T) {
	server := &Server{historyStore: history.New(t.TempDir())}
	body := `{"media_id":"tmdb:1","title":"The Movie","year":2001,"poster_url":"https://image.example/poster.jpg","genres":["Drama","Thriller"],"position_seconds":120,"duration_seconds":1000}`
	request := httptest.NewRequest(http.MethodPut, "/v1/watch-history", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/watch-history?continue=true", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Entries []history.Entry `json:"entries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].ResumePosition() != 120 || len(payload.Entries[0].Genres) != 2 {
		t.Fatalf("entries = %+v", payload.Entries)
	}
}

func TestRemoveWatchHistoryClearsSavedProgress(t *testing.T) {
	store := history.New(t.TempDir())
	entry, err := store.RecordProgress(history.Entry{
		MediaID:         "tmdb:1",
		Title:           "The Movie",
		Year:            2001,
		PositionSeconds: 120,
		DurationSeconds: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{historyStore: store}
	request := httptest.NewRequest(http.MethodDelete, "/v1/watch-history/"+entry.ID, nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries after removal = %+v", entries)
	}
}

func TestCompletedEpisodeContinuesWithFirstUnwatchedEpisode(t *testing.T) {
	store := history.New(t.TempDir())
	_, err := store.RecordProgress(history.Entry{
		MediaID: "tmdb-tv:3:s1:e1", MediaType: "show", Title: "Top Rated Show",
		SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show", SeasonNumber: 1,
		EpisodeNumber: 1, EpisodeTitle: "Pilot", PositionSeconds: 980, DurationSeconds: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{historyStore: store, metadataProvider: fakeMetadata{}}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/watch-history?continue=true", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Entries []history.Entry `json:"entries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].MediaID != "tmdb-tv:3:s1:e2" || payload.Entries[0].EpisodeTitle != "Second" || payload.Entries[0].PositionSeconds != 0 {
		t.Fatalf("continue entries = %+v", payload.Entries)
	}
}

func TestEpisodeHistoryAndSeriesRemoval(t *testing.T) {
	store := history.New(t.TempDir())
	server := &Server{historyStore: store}
	for episode := 1; episode <= 2; episode++ {
		body := fmt.Sprintf(`{"media_id":"tmdb-tv:3:s1:e%d","media_type":"show","title":"Top Rated Show","year":2020,"series_id":"tmdb-tv:3","series_title":"Top Rated Show","season_number":1,"episode_number":%d,"episode_title":"Episode %d","position_seconds":120,"duration_seconds":1000}`, episode, episode, episode)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/watch-history", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("update status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	entries, err := store.List()
	if err != nil || len(entries) != 2 || entries[0].SeriesID != "tmdb-tv:3" {
		t.Fatalf("episode entries = %+v, error = %v", entries, err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/v1/watch-history/ignored?series_id=tmdb-tv%3A3", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", response.Code, response.Body.String())
	}
	entries, err = store.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries after series removal = %+v, error = %v", entries, err)
	}
}

func TestWatchHistoryAddsMissingMetadata(t *testing.T) {
	store := history.New(t.TempDir())
	entry, err := store.RecordProgress(history.Entry{
		Title: "The Movie", Year: 2001, PositionSeconds: 120, DurationSeconds: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{historyStore: store, metadataProvider: fakeMetadata{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/watch-history?continue=true", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "poster.jpg") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].MediaID != "tmdb:1" || len(entries[0].Genres) != 2 || entries[0].UpdatedAt != entry.UpdatedAt {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestUsenetCandidateFailuresAreTemporarilySkipped(t *testing.T) {
	server := &Server{usenetFailures: make(map[string]time.Time)}
	candidate := catalog.Candidate{ID: "release-1", Indexer: "usenet"}
	if server.usenetCandidateRecentlyFailed(candidate) {
		t.Fatal("new candidate was marked failed")
	}
	server.markUsenetCandidateFailed(candidate)
	if !server.usenetCandidateRecentlyFailed(candidate) {
		t.Fatal("failed candidate was not skipped")
	}
	server.clearUsenetCandidateFailure(candidate)
	if server.usenetCandidateRecentlyFailed(candidate) {
		t.Fatal("successful candidate remained marked failed")
	}
}

func TestFirstTextSubtitleOptionPrefersLiveSupportedRelease(t *testing.T) {
	manager := &fakeHLSManager{subtitleTracks: map[string][]hls.SubtitleTrack{
		"with-subtitles": {{Index: 3, Language: "en"}},
	}}
	server := &Server{
		hlsManager: manager,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	options := []playbackOption{
		{session: &torrentstream.Session{ID: "without-subtitles"}, peers: 12},
		{session: &torrentstream.Session{ID: "with-subtitles"}, peers: 5},
	}
	if index := server.firstTextSubtitleOption(t.Context(), options); index != 1 {
		t.Fatalf("subtitle option = %d, want 1", index)
	}
}

func TestHLSAssetsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "segment-000000.m4s"), []byte("segment"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subtitle-6.vtt"), []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := &fakeHLSManager{dir: dir}
	server := &Server{hlsManager: manager}

	request := httptest.NewRequest(http.MethodPost, "/v1/playbacks/abc/hls/subtitles/6", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || manager.subtitlePlayback != "abc" || manager.subtitleIndex != 6 {
		t.Fatalf("subtitle start status = %d, playback = %q, index = %d", response.Code, manager.subtitlePlayback, manager.subtitleIndex)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/playbacks/abc/hls/index.m3u8", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("status = %d, content type = %q", response.Code, response.Header().Get("Content-Type"))
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("playlist cache control = %q", response.Header().Get("Cache-Control"))
	}
	if !strings.Contains(response.Body.String(), "#EXT-X-START:TIME-OFFSET=0,PRECISE=YES") {
		t.Fatalf("playlist = %q", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/playbacks/abc/hls/segment-000000.m4s", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("segment status = %d, cache control = %q", response.Code, response.Header().Get("Cache-Control"))
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/playbacks/abc/hls/subtitle-6.vtt", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
		t.Fatalf("subtitle status = %d, content type = %q", response.Code, response.Header().Get("Content-Type"))
	}

	request = httptest.NewRequest(http.MethodDelete, "/v1/playbacks/abc/hls", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || manager.stopped != "abc" {
		t.Fatalf("status = %d, stopped = %q", response.Code, manager.stopped)
	}
}

func TestPublicRankedCandidateRemovesPrivateTorrentSources(t *testing.T) {
	candidate := publicRankedCandidate(catalog.RankedCandidate{Candidate: catalog.Candidate{
		Name:       "The Movie",
		MagnetURI:  "magnet:?xt=private",
		TorrentURL: "https://indexer.example/download?apikey=secret",
	}})
	if candidate.Candidate.MagnetURI != "" || candidate.Candidate.TorrentURL != "" {
		t.Fatalf("public candidate contains private sources: %+v", candidate.Candidate)
	}
	if candidate.Candidate.Name != "The Movie" {
		t.Fatalf("candidate name = %q", candidate.Candidate.Name)
	}
}

func TestRequestSchemeUsesForwardedProtocol(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	if got := requestScheme(request); got != "https" {
		t.Fatalf("scheme = %q", got)
	}
}

func TestReloadIndexerConfiguration(t *testing.T) {
	called := false
	server := &Server{reloadIndexers: func() error {
		called = true
		return nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/indexers/reload", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !called {
		t.Fatal("reload callback was not called")
	}
}
