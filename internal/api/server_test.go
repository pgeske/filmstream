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
	"sync"
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

type imdbDiscoveryMetadata struct {
	fakeMetadata
	calls int
}

type airingMetadata struct{}

type fakeRatings struct{}

type partialRatings struct{}

type fakeTorrentPlaybackEngine struct {
	mu                sync.Mutex
	created           []torrentstream.Source
	dropped           []string
	sessions          map[string]*torrentstream.Session
	statuses          map[string]torrentstream.Status
	sourceUnavailable map[string]error
}

func (f *fakeTorrentPlaybackEngine) Create(_ context.Context, source torrentstream.Source) (*torrentstream.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("playback-%d", len(f.created)+1)
	session := &torrentstream.Session{ID: id, Name: id, FileName: "episode.mkv"}
	f.created = append(f.created, source)
	if f.sessions == nil {
		f.sessions = make(map[string]*torrentstream.Session)
	}
	f.sessions[id] = session
	return session, nil
}

func (f *fakeTorrentPlaybackEngine) Get(id string) (*torrentstream.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[id]
	return session, ok
}

func (*fakeTorrentPlaybackEngine) TorrentMetainfo(string) ([]byte, error) {
	return nil, nil
}

func (f *fakeTorrentPlaybackEngine) Drop(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, id)
	delete(f.sessions, id)
	return nil
}

func (f *fakeTorrentPlaybackEngine) Status(id string) (torrentstream.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, ok := f.statuses[id]
	return status, ok
}

func (f *fakeTorrentPlaybackEngine) SourceUnavailable(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sourceUnavailable[id]
}

func (*fakeTorrentPlaybackEngine) ServeHTTP(http.ResponseWriter, *http.Request, string) error {
	return nil
}

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
	bitmapSubtitle   int
	startErrors      map[string]error
}

func (f *fakeHLSManager) ProbeSubtitles(_ context.Context, id string) ([]hls.SubtitleTrack, error) {
	if err := f.probeErrors[id]; err != nil {
		return nil, err
	}
	return f.subtitleTracks[id], nil
}

func (f *fakeHLSManager) Start(_ context.Context, id string, start float64, languages []string, bitmapSubtitle int) (hls.Stream, error) {
	f.languages = append([]string(nil), languages...)
	f.bitmapSubtitle = bitmapSubtitle
	if err := f.startErrors[id]; err != nil {
		return hls.Stream{}, err
	}
	return hls.Stream{
		PlaybackID: id, StartSeconds: start, VideoCodec: "h264",
		BurnedSubtitleIndex: optionalTestIndex(bitmapSubtitle),
	}, nil
}

func optionalTestIndex(index int) *int {
	if index < 0 {
		return nil
	}
	return &index
}

func (f *fakeHLSManager) StartSubtitle(_ context.Context, playbackID string, index int) error {
	f.subtitlePlayback = playbackID
	f.subtitleIndex = index
	return nil
}

func (f *fakeHLSManager) AssetPath(_, name string) (string, error) {
	return filepath.Join(f.dir, name), nil
}

func (f *fakeHLSManager) Prepared(string, float64, []string, int, int) bool {
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

func (fakeRatings) RatingsForMedia(_ context.Context, _ metadata.Movie) (metadata.MovieRatings, error) {
	imdb := 8.6
	votes := 123456
	return metadata.MovieRatings{IMDb: &imdb, IMDbVotes: &votes}, nil
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

func (provider *imdbDiscoveryMetadata) DiscoverWithRatings(ctx context.Context, collection metadata.Collection, _ metadata.MediaRatingsProvider) ([]metadata.Movie, error) {
	provider.calls++
	return provider.Discover(ctx, collection)
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
		Seasons: []metadata.SeasonSummary{
			{Number: 1, Name: "Season 1", EpisodeCount: 2},
			{Number: 2, Name: "Season 2", EpisodeCount: 3},
		},
	}, nil
}

func (fakeMetadata) Season(_ context.Context, id string, number int) (metadata.Season, error) {
	if id != "tmdb-tv:3" {
		return metadata.Season{}, errors.New("season not found")
	}
	season := metadata.Season{
		SeriesID: id, SeriesTitle: "Top Rated Show", Number: number,
		Name: fmt.Sprintf("Season %d", number),
	}
	switch number {
	case 1:
		season.Episodes = []metadata.Episode{
			{
				ID: "tmdb-tv:3:s1:e1", SeriesID: id, SeriesTitle: "Top Rated Show",
				SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot",
			},
			{
				ID: "tmdb-tv:3:s1:e2", SeriesID: id, SeriesTitle: "Top Rated Show",
				SeasonNumber: 1, EpisodeNumber: 2, Title: "Second",
			},
		}
	case 2:
		season.Episodes = []metadata.Episode{
			{
				ID: "tmdb-tv:3:s2:e1", SeriesID: id, SeriesTitle: "Top Rated Show",
				SeasonNumber: 2, EpisodeNumber: 1, Title: "Season Two Premiere",
			},
			{
				ID: "tmdb-tv:3:s2:e3", SeriesID: id, SeriesTitle: "Top Rated Show",
				SeasonNumber: 2, EpisodeNumber: 3, Title: "Season Two Third",
			},
			{
				ID: "tmdb-tv:3:s2:e4", SeriesID: id, SeriesTitle: "Top Rated Show",
				SeasonNumber: 2, EpisodeNumber: 4, Title: "Future Episode", AirDate: "2999-01-01",
			},
		}
	default:
		return metadata.Season{}, errors.New("season not found")
	}
	return season, nil
}

func (airingMetadata) Search(_ context.Context, _ string) ([]metadata.Movie, error) {
	return nil, nil
}

func (airingMetadata) Show(_ context.Context, id string) (metadata.Show, error) {
	return metadata.Show{
		Movie:   metadata.Movie{ID: id, MediaType: metadata.MediaTypeShow, Title: "Current Show", NumberOfSeasons: 1},
		Seasons: []metadata.SeasonSummary{{Number: 1, Name: "Season 1", EpisodeCount: 10}},
	}, nil
}

func (airingMetadata) Season(_ context.Context, id string, number int) (metadata.Season, error) {
	return metadata.Season{
		SeriesID: id, SeriesTitle: "Current Show", Number: number,
		Episodes: []metadata.Episode{
			{ID: id + ":s1:e1", SeriesID: id, SeasonNumber: 1, EpisodeNumber: 1},
			{ID: id + ":s1:e2", SeriesID: id, SeasonNumber: 1, EpisodeNumber: 2},
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

func TestCreateEpisodePlaybackUsesTorrentSeasonPack(t *testing.T) {
	dataDir := t.TempDir()
	torrentContents := createAPITestTorrentForFile(t, dataDir, "Original.Show.S01E02.mp4")
	var tvSearchQuery string
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/season" {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(torrentContents)
			return
		}
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`)
		case "tvsearch":
			tvSearchQuery = r.URL.RawQuery
			if r.URL.Query().Get("q") == "Original Show" {
				fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Original.Show.S01.Complete.1080p.WEB.H264-GROUP</title><guid>season-1</guid><enclosure url="/download/season" length="607836000" type="application/x-bittorrent"/></item></channel></rss>`)
			} else {
				fmt.Fprint(w, `<?xml version="1.0"?><rss><channel/></rss>`)
			}
		case "search":
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel/></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: indexerServer.URL + "/api", APIKey: "secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: dataDir, MetadataTimeout: time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{Codecs: []string{"h264"}}, slog.Default())
	fakeUsenet := &fakeUsenetPlaybackEngine{session: &usenetstream.Session{ID: "should-not-be-used"}}
	server.usenetEngine = fakeUsenet
	server.playbackSourceMode = config.PlaybackSourceUsenetOnly

	body := `{"media_id":"tmdb-tv:3:s1:e2","media_type":"show","query":"Localized Name","original_title":"Original Show","year":2020,"series_id":"tmdb-tv:3","series_title":"Localized Name","season_number":1,"episode_number":2,"episode_title":"Second","preferences":{"codecs":["h264"]}}`
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload CreatePlaybackResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != catalog.ProtocolTorrent || payload.FileName != "Original.Show.S01E02.mp4" {
		t.Fatalf("payload = %+v", payload)
	}
	if fakeUsenet.createdSource != (usenetstream.Source{}) {
		t.Fatalf("Usenet source was used: %+v", fakeUsenet.createdSource)
	}
	if !strings.Contains(tvSearchQuery, "season=1") || strings.Contains(tvSearchQuery, "ep=2") || !strings.Contains(tvSearchQuery, "q=Original+Show") {
		t.Fatalf("TV search query = %q", tvSearchQuery)
	}
	if got := server.playbackCacheKeys[payload.ID].mediaID; got != "tmdb-tv:3:s1" {
		t.Fatalf("playback cache media ID = %q", got)
	}
}

func TestTorrentPlaybackCacheIsSharedAcrossSeason(t *testing.T) {
	store := playbackcache.New(t.TempDir())
	firstEpisode := CreatePlaybackRequest{
		MediaID: "tmdb-tv:3:s1:e2", SeriesID: "tmdb-tv:3", SeasonNumber: 1,
		Query: "Top Rated Show", Year: 2020,
	}
	nextEpisode := firstEpisode
	nextEpisode.MediaID = "tmdb-tv:3:s1:e3"
	selected := catalog.RankedCandidate{
		Candidate: catalog.Candidate{
			Name: "Top.Rated.Show.S01.Complete.1080p", Protocol: catalog.ProtocolTorrent,
		},
		Reasons: []string{"season pack"},
	}
	cacheID := playbackCacheMediaID(firstEpisode, catalog.ProtocolTorrent, &selected)
	if _, err := store.Save(cacheID, firstEpisode.Query, firstEpisode.Year, selected, []byte("torrent")); err != nil {
		t.Fatal(err)
	}
	cached, found, err := store.Lookup(
		playbackCacheMediaID(nextEpisode, catalog.ProtocolTorrent, &selected),
		nextEpisode.Query,
		nextEpisode.Year,
	)
	if err != nil || !found || cached.Selected.Candidate.Name != selected.Candidate.Name {
		t.Fatalf("cached = %+v, found = %v, error = %v", cached, found, err)
	}

	individual := selected
	individual.Reasons = []string{"exact episode"}
	if firstID, nextID := playbackCacheMediaID(firstEpisode, catalog.ProtocolTorrent, &individual),
		playbackCacheMediaID(nextEpisode, catalog.ProtocolTorrent, &individual); firstID == nextID {
		t.Fatalf("individual episode releases share cache ID %q", firstID)
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

// A persisted release must be replayed without re-searching even when the freshly
// mounted swarm has not connected any peers yet; otherwise slow tracker or DHT
// startup would evict a known-good release and force a full search on every replay.
func TestCreatePlaybackReusesCachedTorrentReleaseWithoutSearching(t *testing.T) {
	dataDir := t.TempDir()
	torrentContents := createAPITestTorrentForFile(t, dataDir, "Original.Show.S01E02.mp4")
	var searches atomic.Int32
	var indexerServer *httptest.Server
	indexerServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/season" {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			_, _ = w.Write(torrentContents)
			return
		}
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`)
		case "tvsearch":
			searches.Add(1)
			fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Original.Show.S01.Complete.1080p.WEB.H264-GROUP</title><guid>season-1</guid><enclosure url="`+indexerServer.URL+`/download/season" length="607836000" type="application/x-bittorrent"/></item></channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: indexerServer.URL + "/api", APIKey: "secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	torrentEngine, err := torrentstream.New(torrentstream.Config{
		DataDir: dataDir, MetadataTimeout: time.Second, CleanOnClose: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer torrentEngine.Close()
	server := New(registry, torrentEngine, catalog.Preferences{Codecs: []string{"h264"}}, slog.Default())
	server.playbackSourceMode = config.PlaybackSourceTorrentOnly
	server.hlsManager = &fakeHLSManager{}
	store := playbackcache.New(t.TempDir())
	server.SetPlaybackCache(store)

	selected := catalog.RankedCandidate{
		Candidate: catalog.Candidate{
			Name: "Original.Show.S01.Complete.1080p.WEB.H264-GROUP", Protocol: catalog.ProtocolTorrent,
		},
		Reasons: []string{"season pack", subtitlesVerifiedReason},
	}
	if _, err := store.Save("tmdb-tv:3:s1", "Original Show", 2020, selected, torrentContents); err != nil {
		t.Fatal(err)
	}

	create := func() CreatePlaybackResponse {
		request := httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(
			`{"media_id":"tmdb-tv:3:s1:e2","media_type":"show","query":"Original Show","year":2020,"series_id":"tmdb-tv:3","series_title":"Original Show","season_number":1,"episode_number":2,"episode_title":"Second","preferences":{"codecs":["h264"],"prefer_text_subtitles":true}}`,
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
	if searches.Load() != 0 {
		t.Fatalf("cached replay searched indexers %d times, want 0", searches.Load())
	}
	if first.Source != catalog.ProtocolTorrent || first.FileName != "Original.Show.S01E02.mp4" {
		t.Fatalf("payload = %+v", first)
	}

	hlsResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(hlsResponse, httptest.NewRequest(
		http.MethodPost, "/v1/playbacks/"+first.ID+"/hls", strings.NewReader(`{}`),
	))
	if hlsResponse.Code != http.StatusCreated {
		t.Fatalf("HLS status = %d, body = %s", hlsResponse.Code, hlsResponse.Body.String())
	}
	if _, found, err := store.Lookup("tmdb-tv:3:s1", "Original Show", 2020); err != nil || !found {
		t.Fatalf("cached release kept after replay: found = %v, error = %v", found, err)
	}

	create()
	if searches.Load() != 0 {
		t.Fatalf("second replay searched indexers %d times, want 0", searches.Load())
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

func TestNextEpisodePrewarmKeepsThirtySecondBuffer(t *testing.T) {
	if prewarmBufferSeconds != 30 {
		t.Fatalf("prewarm buffer = %d", prewarmBufferSeconds)
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

func TestShowPrewarmCachesReleaseSearchWithoutCreatingPlayback(t *testing.T) {
	var searchRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`)
		case "tvsearch", "search":
			searchRequests.Add(1)
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>The.Show.S01.Complete.1080p.WEB.H264</title><guid>pack</guid><enclosure url="/pack" length="1000" type="application/x-bittorrent"/><attr name="seeders" value="50"/></item></channel></rss>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: upstream.URL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		indexers: registry,
		defaults: catalog.Preferences{
			Resolution: "1080p", Codecs: []string{"h264", "h265"}, StreamingOptimized: true,
		},
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		prewarmStates:   make(map[string]*playbackPrewarmState),
		prewarmSlots:    make(chan struct{}, 1),
		releaseSearches: make(map[string]*releaseSearchState),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx

	httpRequest := httptest.NewRequest(http.MethodPost, "/v1/playbacks/prewarm", strings.NewReader(
		`{"media_id":"tmdb-tv:3:s1:e1","media_type":"show","query":"The Show","series_id":"tmdb-tv:3","season_number":1,"episode_number":1,"preferences":{"prefer_text_subtitles":true}}`,
	))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httpRequest)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	request := CreatePlaybackRequest{
		MediaID: "tmdb-tv:3:s1:e1", MediaType: "show", Query: "The Show",
		SeriesID: "tmdb-tv:3", SeasonNumber: 1, EpisodeNumber: 1,
		Preferences: mergePreferences(server.defaults, catalog.Preferences{PreferTextSubtitles: true}),
	}
	lookupContext, lookupCancel := context.WithTimeout(t.Context(), time.Second)
	defer lookupCancel()
	ranked, found := server.cachedShowReleases(lookupContext, request)
	if !found || len(ranked) != 1 || !catalog.IsSeasonPack(ranked[0].Candidate.Name, 1) {
		t.Fatalf("ranked = %+v, found = %v", ranked, found)
	}
	if searchRequests.Load() == 0 || len(server.prewarmStates) != 0 {
		t.Fatalf("search requests = %d, playback prewarms = %d", searchRequests.Load(), len(server.prewarmStates))
	}
}

func TestSeasonPackPreferenceAllowsCurrentIncompleteSeasonFallback(t *testing.T) {
	current := &Server{metadataProvider: airingMetadata{}}
	request := CreatePlaybackRequest{SeriesID: "tmdb-tv:9", SeasonNumber: 1}
	if current.shouldPreferSeasonPack(t.Context(), request) {
		t.Fatal("incomplete current season unexpectedly required a season pack")
	}

	older := &Server{metadataProvider: fakeMetadata{}}
	request = CreatePlaybackRequest{SeriesID: "tmdb-tv:3", SeasonNumber: 1}
	if !older.shouldPreferSeasonPack(t.Context(), request) {
		t.Fatal("completed older season did not prefer a season pack")
	}
}

func TestAutoplayClaimWaitsForCanceledPrewarmParkBeforeOldPlaybackCloses(t *testing.T) {
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
	parkCanceled := make(chan struct{})
	parkDone := make(chan struct{})
	request := CreatePlaybackRequest{MediaID: "tmdb:1", Query: "The Movie", StartSeconds: 600}
	server.prewarmStates["tmdb:1"] = &playbackPrewarmState{
		target: playbackPrewarmTarget{request: request}, playbackReady: ready,
		response: CreatePlaybackResponse{ID: "warm-playback"},
		cancel:   func() {}, parkCancel: func() { close(parkCanceled) }, parkDone: parkDone,
	}

	type claimResult struct {
		response CreatePlaybackResponse
		ok       bool
	}
	claimed := make(chan claimResult, 1)
	go func() {
		response, ok := server.claimPrewarmedPlayback(t.Context(), request)
		claimed <- claimResult{response: response, ok: ok}
	}()
	select {
	case <-parkCanceled:
	case <-time.After(time.Second):
		t.Fatal("autoplay did not cancel the pending park")
	}
	select {
	case result := <-claimed:
		t.Fatalf("claim returned before park exited: %+v", result)
	default:
	}

	// The next player may start only after the canceled prewarm can no longer park it.
	close(parkDone)
	result := <-claimed
	if !result.ok || result.response.ID != "warm-playback" {
		t.Fatalf("response = %+v, claimed = %v", result.response, result.ok)
	}

	// SwiftUI tears the completed player down after replacing it. Its close must
	// remain scoped to the old playback rather than canceling the claimed episode.
	manager := &fakeHLSManager{}
	server.hlsManager = manager
	closeResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(closeResponse, httptest.NewRequest(
		http.MethodDelete, "/v1/playbacks/old-playback/hls", nil,
	))
	if closeResponse.Code != http.StatusOK || manager.stopped != "old-playback" ||
		result.response.ID != "warm-playback" {
		t.Fatalf("close status = %d, stopped = %q, next = %q",
			closeResponse.Code, manager.stopped, result.response.ID)
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

func TestDuplicateProgressKeepsOneNextEpisodeSourcePrewarm(t *testing.T) {
	var createRequests atomic.Int32
	parked := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/playbacks":
			createRequests.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"next-playback","name":"The Show","file_name":"episode.mkv","file_size":1000,"source":"torrent","stream_url":"http://127.0.0.1/stream"}`)
		case "/v1/playbacks/next-playback/hls":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case "/v1/playbacks/next-playback/hls/park":
			select {
			case parked <- struct{}{}:
			default:
			}
			fmt.Fprint(w, `{"status":"parked"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server := &Server{
		metadataProvider: fakeMetadata{},
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		prewarmStates:    make(map[string]*playbackPrewarmState),
		prewarmSlots:     make(chan struct{}, 1),
		prewarmBaseURL:   upstream.URL,
		prewarmClient:    upstream.Client(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx
	current := history.Entry{
		MediaID: "tmdb-tv:3:s1:e1", MediaType: "show", Title: "Top Rated Show",
		SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show", SeasonNumber: 1, EpisodeNumber: 1,
	}
	selection := playbackSubtitleSelection{Mode: "off"}
	server.queueNextEpisodePrewarm(current, selection)
	server.queueNextEpisodePrewarm(current, selection)

	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("next-episode prewarm did not finish")
	}
	if createRequests.Load() != 1 {
		t.Fatalf("next-episode source creations = %d, want 1", createRequests.Load())
	}
}

func TestClaimedPlaybackBlocksDuplicateSourcePrewarm(t *testing.T) {
	var createRequests atomic.Int32
	parked := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/playbacks":
			createRequests.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"warm-playback","name":"The Show","file_name":"episode.mkv","file_size":1000,"source":"torrent","stream_url":"http://127.0.0.1/stream"}`)
		case "/v1/playbacks/warm-playback/hls":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case "/v1/playbacks/warm-playback/hls/park":
			select {
			case parked <- struct{}{}:
			default:
			}
			fmt.Fprint(w, `{"status":"parked"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server := &Server{
		metadataProvider: fakeMetadata{},
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		prewarmStates:    make(map[string]*playbackPrewarmState),
		claimedPlaybacks: make(map[string]claimedPlayback),
		prewarmSlots:     make(chan struct{}, 1),
		prewarmBaseURL:   upstream.URL,
		prewarmClient:    upstream.Client(),
		usenetEngine: &fakeUsenetPlaybackEngine{
			session: &usenetstream.Session{ID: "warm-playback", Name: "The Show", FileName: "episode.mkv"},
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx
	current := history.Entry{
		MediaID: "tmdb-tv:3:s1:e1", MediaType: "show", Title: "Top Rated Show",
		SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show", SeasonNumber: 1, EpisodeNumber: 1,
	}
	selection := playbackSubtitleSelection{Mode: "off"}

	// The client claims the prewarmed next episode and starts streaming it.
	ready := make(chan struct{})
	close(ready)
	claimRequest := CreatePlaybackRequest{
		MediaID: "tmdb-tv:3:s1:e2", MediaType: "show", Query: "Top Rated Show",
		SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show",
		SeasonNumber: 1, EpisodeNumber: 2, StartSeconds: 0,
		Preferences: catalog.Preferences{Languages: []string{"ja", "en", "english"}},
	}
	server.prewarmStates["tmdb-tv:3:s1:e2"] = &playbackPrewarmState{
		target:        playbackPrewarmTarget{request: claimRequest},
		playbackReady: ready,
		response:      CreatePlaybackResponse{ID: "warm-playback"},
		cancel:        func() {},
	}
	claimed, ok := server.claimPrewarmedPlayback(t.Context(), claimRequest)
	if !ok || claimed.ID != "warm-playback" {
		t.Fatalf("claimed = %+v, ok = %v", claimed, ok)
	}

	// The end-of-episode progress report re-requests the same prewarm. It must
	// not mount a second source session behind the actively playing episode.
	server.queueNextEpisodePrewarm(current, selection)
	if createRequests.Load() != 0 {
		t.Fatalf("duplicate next-episode creations = %d, want 0", createRequests.Load())
	}

	// Backing out stops the claimed stream and unblocks prewarming again.
	closeResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(closeResponse, httptest.NewRequest(
		http.MethodDelete, "/v1/playbacks/warm-playback/hls", nil,
	))
	if closeResponse.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", closeResponse.Code, closeResponse.Body.String())
	}

	server.queueNextEpisodePrewarm(current, selection)
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("next-episode prewarm did not restart after the claimed stream closed")
	}
	if createRequests.Load() != 1 {
		t.Fatalf("next-episode creations after close = %d, want 1", createRequests.Load())
	}
}

func TestBitmapSubtitlePrewarmMatchesMetadataAcrossTrackIndexes(t *testing.T) {
	selection := playbackSubtitleSelection{
		Mode: "bitmap", Index: 6, Language: "en", Title: "English SDH",
		Codec: "hdmv_pgs_subtitle",
	}
	tracks := []hls.SubtitleTrack{
		{Index: 6, Language: "ja", Title: "Japanese", Codec: "hdmv_pgs_subtitle", Kind: "bitmap"},
		{Index: 3, Language: "en", Title: "English", Codec: "subrip", Kind: "text"},
		{Index: 8, Language: "en", Title: "English", Codec: "hdmv_pgs_subtitle", Kind: "bitmap"},
		{Index: 9, Language: "en", Title: "English SDH", Codec: "hdmv_pgs_subtitle", Kind: "bitmap"},
	}
	if index := matchingBitmapSubtitleIndex(tracks, selection); index != 9 {
		t.Fatalf("matching bitmap subtitle index = %d, want 9", index)
	}
	if index := matchingBitmapSubtitleIndex(tracks[:2], selection); index != -1 {
		t.Fatalf("unavailable bitmap subtitle index = %d, want -1", index)
	}
	selection.Mode = "text"
	if index := matchingBitmapSubtitleIndex(tracks, selection); index != -1 {
		t.Fatalf("text subtitle prewarm index = %d, want -1", index)
	}
}

func TestPlaybackPrewarmStartsMatchedBitmapSubtitle(t *testing.T) {
	hlsRequests := make(chan struct {
		startSeconds   float64
		bitmapSubtitle int
	}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/playbacks":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"warm-playback","name":"The Show","file_name":"episode.mkv","file_size":1000,"source":"usenet","stream_url":"http://127.0.0.1/stream"}`)
		case "/v1/playbacks/warm-playback/hls":
			var request struct {
				StartSeconds        float64 `json:"start_seconds"`
				BitmapSubtitleIndex int     `json:"bitmap_subtitle_index"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			hlsRequests <- struct {
				startSeconds   float64
				bitmapSubtitle int
			}{request.StartSeconds, request.BitmapSubtitleIndex}
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
		session: &usenetstream.Session{ID: "warm-playback", Name: "The Show", FileName: "episode.mkv"},
	}
	server.hlsManager = &fakeHLSManager{subtitleTracks: map[string][]hls.SubtitleTrack{
		"warm-playback": {
			{Index: 6, Language: "ja", Title: "Japanese", Codec: "hdmv_pgs_subtitle", Kind: "bitmap"},
			{Index: 9, Language: "en", Title: "English SDH", Codec: "hdmv_pgs_subtitle", Kind: "bitmap"},
		},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx
	server.prewarmBaseURL = upstream.URL
	server.prewarmClient = upstream.Client()
	server.queuePlaybackPrewarm(playbackPrewarmTarget{
		request: CreatePlaybackRequest{
			MediaID: "tmdb-tv:3:s1:e2", MediaType: "show", Query: "The Show",
			SeriesID: "tmdb-tv:3", SeasonNumber: 1, EpisodeNumber: 2,
		},
		priority: true,
		subtitleSelection: playbackSubtitleSelection{
			Mode: "bitmap", Index: 6, Language: "en", Title: "English SDH",
			Codec: "hdmv_pgs_subtitle",
		},
	})

	select {
	case request := <-hlsRequests:
		if request.startSeconds != 0 || request.bitmapSubtitle != 9 {
			t.Fatalf("HLS prewarm request = %+v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HLS prewarm request")
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

func TestTransientCachedUsenetFailureKeepsKnownGoodRelease(t *testing.T) {
	store := playbackcache.New(t.TempDir())
	candidate := catalog.RankedCandidate{Candidate: catalog.Candidate{
		ID: "release-1", Indexer: "usenet", Name: "Movie.1080p", Protocol: catalog.ProtocolUsenet,
	}}
	if _, err := store.SaveUsenet("tmdb:1", "Movie", 2001, candidate, []byte("nzb")); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		usenetEngine: &fakeUsenetPlaybackEngine{
			createErr: fmt.Errorf("browse prepared release: %w", context.DeadlineExceeded),
		},
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
	if _, found, err := store.LookupUsenet("tmdb:1", "Movie", 2001); err != nil || !found {
		t.Fatalf("cached Usenet playback found = %v, error = %v", found, err)
	}
	if server.usenetCandidateRecentlyFailed(candidate.Candidate) {
		t.Fatal("transient failure quarantined a known-good release")
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
	return createAPITestTorrentForFile(t, dataDir, "Sintel.mp4")
}

func createAPITestTorrentForFile(t *testing.T, dataDir, fileName string) []byte {
	t.Helper()
	torrentDataDir := filepath.Join(dataDir, "torrents")
	if err := os.MkdirAll(torrentDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	videoPath := filepath.Join(torrentDataDir, fileName)
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

func TestDiscoverCatalogUsesIMDbAwareProviderWhenRatingsAreConfigured(t *testing.T) {
	provider := &imdbDiscoveryMetadata{}
	server := &Server{metadataProvider: provider, ratingsProvider: fakeRatings{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/discover", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if provider.calls != 2 {
		t.Fatalf("IMDb-aware discovery calls = %d, want 2", provider.calls)
	}
}

func TestShowCatalogReturnsSeasonsAndEpisodes(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/catalog/shows/tmdb-tv:3", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"number_of_seasons":3`) || !strings.Contains(response.Body.String(), `"episode_count":2`) {
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

func TestRecentSeasonProgressDoesNotBackfillEarlierUnwatchedSeasons(t *testing.T) {
	tests := []struct {
		name             string
		position         float64
		wantMediaID      string
		wantStartSeconds float64
	}{
		{name: "partial episode resumes", position: 500, wantMediaID: "tmdb-tv:3:s2:e1", wantStartSeconds: 500},
		{name: "completed episode advances across a numbering gap", position: 950, wantMediaID: "tmdb-tv:3:s2:e3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := history.New(t.TempDir())
			_, err := store.RecordProgress(history.Entry{
				MediaID: "tmdb-tv:3:s2:e1", MediaType: "show", Title: "Top Rated Show",
				SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show", SeasonNumber: 2,
				EpisodeNumber: 1, EpisodeTitle: "Season Two Premiere",
				PositionSeconds: test.position, DurationSeconds: 1000,
			})
			if err != nil {
				t.Fatal(err)
			}
			entries := requestContinueWatchHistory(t, &Server{historyStore: store, metadataProvider: fakeMetadata{}})
			if len(entries) != 1 || entries[0].MediaID != test.wantMediaID || entries[0].PositionSeconds != test.wantStartSeconds {
				t.Fatalf("continue entries = %+v", entries)
			}
		})
	}
}

func TestCompletedEpisodeContinuesAcrossSeasonBoundary(t *testing.T) {
	store := history.New(t.TempDir())
	_, err := store.RecordProgress(history.Entry{
		MediaID: "tmdb-tv:3:s1:e2", MediaType: "show", Title: "Top Rated Show",
		SeriesID: "tmdb-tv:3", SeriesTitle: "Top Rated Show", SeasonNumber: 1,
		EpisodeNumber: 2, EpisodeTitle: "Second", PositionSeconds: 950, DurationSeconds: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := requestContinueWatchHistory(t, &Server{historyStore: store, metadataProvider: fakeMetadata{}})
	if len(entries) != 1 || entries[0].MediaID != "tmdb-tv:3:s2:e1" {
		t.Fatalf("continue entries = %+v", entries)
	}
}

func TestEpisodeContinuationUsesLatestMeaningfulActivity(t *testing.T) {
	old := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	recent := old.Add(time.Hour)
	tests := []struct {
		name      string
		entries   []history.Entry
		wantMedia string
		wantStart float64
	}{
		{
			name: "newer completion wins over stale partial progress",
			entries: []history.Entry{
				{ID: "s1-partial", MediaID: "tmdb-tv:3:s1:e2", SeriesID: "tmdb-tv:3", SeasonNumber: 1, EpisodeNumber: 2, PositionSeconds: 400, DurationSeconds: 1000, UpdatedAt: old},
				{ID: "s2-complete", MediaID: "tmdb-tv:3:s2:e1", SeriesID: "tmdb-tv:3", SeasonNumber: 2, EpisodeNumber: 1, PositionSeconds: 950, DurationSeconds: 1000, Completed: true, UpdatedAt: recent},
			},
			wantMedia: "tmdb-tv:3:s2:e3",
		},
		{
			name: "equal timestamps prefer later chronology",
			entries: []history.Entry{
				{ID: "s1-partial", MediaID: "tmdb-tv:3:s1:e2", SeriesID: "tmdb-tv:3", SeasonNumber: 1, EpisodeNumber: 2, PositionSeconds: 400, DurationSeconds: 1000, UpdatedAt: recent},
				{ID: "s2-complete", MediaID: "tmdb-tv:3:s2:e1", SeriesID: "tmdb-tv:3", SeasonNumber: 2, EpisodeNumber: 1, PositionSeconds: 950, DurationSeconds: 1000, Completed: true, UpdatedAt: recent},
			},
			wantMedia: "tmdb-tv:3:s2:e3",
		},
		{
			name: "recent partial replay resumes the older episode",
			entries: []history.Entry{
				{ID: "s2-complete", MediaID: "tmdb-tv:3:s2:e1", SeriesID: "tmdb-tv:3", SeasonNumber: 2, EpisodeNumber: 1, Completed: true, UpdatedAt: old},
				{ID: "s1-replay", MediaID: "tmdb-tv:3:s1:e1", SeriesID: "tmdb-tv:3", SeasonNumber: 1, EpisodeNumber: 1, PositionSeconds: 300, DurationSeconds: 1000, UpdatedAt: recent},
			},
			wantMedia: "tmdb-tv:3:s1:e1", wantStart: 300,
		},
		{
			name: "completed replay advances from the explicit replay anchor",
			entries: []history.Entry{
				{ID: "s2-complete", MediaID: "tmdb-tv:3:s2:e1", SeriesID: "tmdb-tv:3", SeasonNumber: 2, EpisodeNumber: 1, Completed: true, UpdatedAt: old},
				{ID: "s1-replay", MediaID: "tmdb-tv:3:s1:e1", SeriesID: "tmdb-tv:3", SeasonNumber: 1, EpisodeNumber: 1, Completed: true, UpdatedAt: recent},
			},
			wantMedia: "tmdb-tv:3:s1:e2",
		},
	}
	server := &Server{metadataProvider: fakeMetadata{}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := server.continueWatchHistory(t.Context(), test.entries)
			if len(entries) != 1 || entries[0].MediaID != test.wantMedia || entries[0].PositionSeconds != test.wantStart {
				t.Fatalf("continue entries = %+v", entries)
			}
		})
	}
}

func TestCompletedEpisodeDoesNotContinueToUnairedEpisode(t *testing.T) {
	entries := (&Server{metadataProvider: fakeMetadata{}}).continueWatchHistory(t.Context(), []history.Entry{{
		ID: "s2-complete", MediaID: "tmdb-tv:3:s2:e3", SeriesID: "tmdb-tv:3",
		SeasonNumber: 2, EpisodeNumber: 3, Completed: true, UpdatedAt: time.Now(),
	}})
	if len(entries) != 0 {
		t.Fatalf("continue entries = %+v", entries)
	}
}

func requestContinueWatchHistory(t *testing.T, server *Server) []history.Entry {
	t.Helper()
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
	return payload.Entries
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
	if entries = requestContinueWatchHistory(t, server); len(entries) != 0 {
		t.Fatalf("continue entries after series removal = %+v", entries)
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

func TestRankedPlaybackStopsAfterHighestRankedHealthySubtitleRelease(t *testing.T) {
	server, engine := newRankedPlaybackTestServer(t,
		map[string]torrentstream.Status{
			"playback-1": {ActivePeers: minimumLivePeers},
			"playback-2": {ActivePeers: strongLiveSwarmPeers},
		},
		map[string][]hls.SubtitleTrack{
			"playback-1": {{Index: 3, Language: "en"}},
			"playback-2": {{Index: 4, Language: "en"}},
		},
	)

	session, selected, err := server.createRankedPlayback(
		t.Context(), rankedTorrentCandidates(3),
		catalog.Preferences{StreamingOptimized: true, PreferTextSubtitles: true},
		"tmdb-tv:1:s1:e1", "S01E01",
	)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "playback-1" || selected.Candidate.ID != "release-1" {
		t.Fatalf("session = %+v, selected = %+v", session, selected)
	}
	if len(engine.created) != 1 {
		t.Fatalf("mounted torrents = %d, want 1", len(engine.created))
	}
	if !hasRankingReason(*selected, subtitlesVerifiedReason) {
		t.Fatalf("selection reasons = %v", selected.Reasons)
	}
}

func TestRankedPlaybackProgressesPastDeadSwarmWithoutUsingFullWait(t *testing.T) {
	server, engine := newRankedPlaybackTestServer(t,
		map[string]torrentstream.Status{
			"playback-1": {},
			"playback-2": {ActivePeers: minimumLivePeers},
		},
		map[string][]hls.SubtitleTrack{
			"playback-2": {{Index: 3, Language: "en"}},
		},
	)
	server.selectionTiming = playbackSelectionTiming{
		progressiveWait: 30 * time.Millisecond,
		liveSwarmWait:   2 * time.Second,
		statusPoll:      time.Millisecond,
	}

	started := time.Now()
	session, selected, err := server.createRankedPlayback(
		t.Context(), rankedTorrentCandidates(3),
		catalog.Preferences{StreamingOptimized: true, PreferTextSubtitles: true},
		"tmdb-tv:1:s1:e1", "S01E01",
	)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "playback-2" || selected.Candidate.ID != "release-2" {
		t.Fatalf("session = %+v, selected = %+v", session, selected)
	}
	if len(engine.created) != 2 {
		t.Fatalf("mounted torrents = %d, want 2", len(engine.created))
	}
	if !slices.Equal(engine.dropped, []string{"playback-1"}) {
		t.Fatalf("dropped torrents = %v", engine.dropped)
	}
	if elapsed >= time.Second {
		t.Fatalf("selection took %v; it consumed the two-second fallback wait", elapsed)
	}
}

func TestRankedPlaybackSubtitleFallbacks(t *testing.T) {
	tests := []struct {
		name       string
		statuses   map[string]torrentstream.Status
		tracks     map[string][]hls.SubtitleTrack
		candidates int
		wantID     string
		wantMounts int
	}{
		{
			name: "subtitle-less options use strongest live swarm",
			statuses: map[string]torrentstream.Status{
				"playback-1": {ActivePeers: 5},
				"playback-2": {ActivePeers: strongLiveSwarmPeers},
				"playback-3": {},
			},
			candidates: 3, wantID: "release-2", wantMounts: 3,
		},
		{
			name: "scarce weak options use best available swarm",
			statuses: map[string]torrentstream.Status{
				"playback-1": {ActivePeers: 1},
				"playback-2": {ActivePeers: 2},
			},
			candidates: 2, wantID: "release-2", wantMounts: 2,
		},
		{
			name: "later subtitle release survives dead and subtitle-less options",
			statuses: map[string]torrentstream.Status{
				"playback-1": {},
				"playback-2": {ActivePeers: 5},
				"playback-3": {ActivePeers: minimumLivePeers},
			},
			tracks: map[string][]hls.SubtitleTrack{
				"playback-3": {{Index: 3, Language: "en"}},
			},
			candidates: 3, wantID: "release-3", wantMounts: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, engine := newRankedPlaybackTestServer(t, test.statuses, test.tracks)
			session, selected, err := server.createRankedPlayback(
				t.Context(), rankedTorrentCandidates(test.candidates),
				catalog.Preferences{StreamingOptimized: true, PreferTextSubtitles: true},
				"tmdb-tv:1:s1:e1", "S01E01",
			)
			if err != nil {
				t.Fatal(err)
			}
			if session.ID != strings.Replace(test.wantID, "release", "playback", 1) ||
				selected.Candidate.ID != test.wantID {
				t.Fatalf("session = %+v, selected = %+v", session, selected)
			}
			if len(engine.created) != test.wantMounts {
				t.Fatalf("mounted torrents = %d, want %d", len(engine.created), test.wantMounts)
			}
			if test.tracks == nil && !hasRankingReason(*selected, subtitleFallbackReason) {
				t.Fatalf("selection reasons = %v", selected.Reasons)
			}
		})
	}
}

func newRankedPlaybackTestServer(
	t *testing.T,
	statuses map[string]torrentstream.Status,
	tracks map[string][]hls.SubtitleTrack,
) (*Server, *fakeTorrentPlaybackEngine) {
	t.Helper()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: "https://indexer.example/api",
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine := &fakeTorrentPlaybackEngine{statuses: statuses}
	server := &Server{
		indexers: registry,
		engine:   engine,
		hlsManager: &fakeHLSManager{
			subtitleTracks: tracks,
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		selectionTiming: playbackSelectionTiming{
			progressiveWait: 5 * time.Millisecond,
			liveSwarmWait:   40 * time.Millisecond,
			statusPoll:      time.Millisecond,
		},
	}
	return server, engine
}

func rankedTorrentCandidates(count int) []catalog.RankedCandidate {
	candidates := make([]catalog.RankedCandidate, 0, count)
	for i := 1; i <= count; i++ {
		candidates = append(candidates, catalog.RankedCandidate{Candidate: catalog.Candidate{
			ID: fmt.Sprintf("release-%d", i), Indexer: "torrent",
			Name: fmt.Sprintf("Show.S01.Release.%d", i), Protocol: catalog.ProtocolTorrent,
			MagnetURI: fmt.Sprintf("magnet:?xt=urn:btih:%040d", i),
		}})
	}
	return candidates
}

func TestUnavailableTorrentRecoveryRefreshesAndStopsAfterTwoCandidates(t *testing.T) {
	var searches atomic.Int32
	var indexerServer *httptest.Server
	indexerServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`)
		case "tvsearch":
			searches.Add(1)
			fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
				<item><title>The.Show.S01.Complete.1080p.WEB.H264-BAD</title><guid>release-bad</guid><enclosure url="`+indexerServer.URL+`/bad.torrent" length="1000000" type="application/x-bittorrent"/><torznab:attr name="seeders" value="120"/></item>
				<item><title>The.Show.S01.Complete.1080p.WEB.H264-HEALTHY</title><guid>release-healthy</guid><enclosure url="`+indexerServer.URL+`/healthy.torrent" length="1000000" type="application/x-bittorrent"/><torznab:attr name="seeders" value="40"/></item>
			</channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: indexerServer.URL + "/api",
	}})
	if err != nil {
		t.Fatal(err)
	}

	mediaID := "tmdb-tv:3:s1:e2"
	bad := catalog.RankedCandidate{Candidate: catalog.Candidate{
		ID: "release-bad", Indexer: "torrent",
		Name: "The.Show.S01.Complete.1080p.WEB.H264-BAD", Protocol: catalog.ProtocolTorrent,
		Resolution: "1080p", Codec: "h264", SizeBytes: 1_000_000,
	}, Reasons: []string{"season pack"}}
	store := playbackcache.New(t.TempDir())
	if _, err := store.Save("tmdb-tv:3:s1", "The Show", 2020, bad, []byte("metainfo")); err != nil {
		t.Fatal(err)
	}
	unavailable := fmt.Errorf("%w: tracker returned no peers", torrentstream.ErrSourceUnavailable)
	engine := &fakeTorrentPlaybackEngine{statuses: map[string]torrentstream.Status{
		"playback-1": {CachedPercent: 14},
		"playback-2": {},
	}}
	manager := &fakeHLSManager{startErrors: map[string]error{
		"playback-1": unavailable,
		"playback-2": unavailable,
	}}
	server := &Server{
		indexers: registry, engine: engine, hlsManager: manager,
		playbackSourceMode: config.PlaybackSourceTorrentOnly,
		defaults:           catalog.Preferences{Codecs: []string{"h264"}},
		playbackCache:      store,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		selected:           make(map[string]catalog.RankedCandidate),
		playbackCacheKeys:  make(map[string]playbackCacheKey),
		playbackLanguages:  make(map[string][]string),
		playbackRequests:   make(map[string]CreatePlaybackRequest),
		playbackResponses:  make(map[string]CreatePlaybackResponse),
		torrentFailures:    make(map[string]time.Time),
		prewarmStates:      make(map[string]*playbackPrewarmState),
		claimedPlaybacks:   make(map[string]claimedPlayback),
		releaseSearches:    make(map[string]*releaseSearchState),
	}
	requestBody := `{"media_id":"tmdb-tv:3:s1:e2","media_type":"show","query":"The Show","year":2020,"series_id":"tmdb-tv:3","series_title":"The Show","season_number":1,"episode_number":2,"episode_title":"Second","preferences":{"codecs":["h264"]}}`
	request := CreatePlaybackRequest{
		MediaID: mediaID, MediaType: "show", Query: "The Show", Year: 2020,
		SeriesID: "tmdb-tv:3", SeriesTitle: "The Show", SeasonNumber: 1, EpisodeNumber: 2,
		Preferences: catalog.Preferences{Codecs: []string{"h264"}},
	}
	ready := make(chan struct{})
	close(ready)
	server.releaseSearches[showReleaseSearchKey(request)] = &releaseSearchState{
		ready: ready, ranked: []catalog.RankedCandidate{bad}, expiresAt: time.Now().Add(time.Minute),
	}

	create := func() (*CreatePlaybackResponse, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(
			http.MethodPost, "/v1/playbacks", strings.NewReader(requestBody),
		))
		if response.Code != http.StatusCreated {
			return nil, response
		}
		var payload CreatePlaybackResponse
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return &payload, response
	}
	failHLS := func(id string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(
			http.MethodPost, "/v1/playbacks/"+id+"/hls", strings.NewReader(`{}`),
		))
		return response
	}

	first, response := create()
	if first == nil || first.ID != "playback-1" || first.Selected == nil || first.Selected.Candidate.ID != "release-bad" {
		t.Fatalf("first playback = %+v, status = %d, body = %s", first, response.Code, response.Body.String())
	}
	if response := failHLS(first.ID); response.Code != http.StatusBadGateway {
		t.Fatalf("first HLS status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, found, err := store.Lookup("tmdb-tv:3:s1", "The Show", 2020); err != nil || found {
		t.Fatalf("unavailable cached release found = %v, error = %v", found, err)
	}
	if _, found := server.releaseSearches[showReleaseSearchKey(request)]; found {
		t.Fatal("stale prefetched release search survived source failure")
	}

	second, response := create()
	if second == nil || second.ID != "playback-2" || second.Selected == nil || second.Selected.Candidate.ID != "release-healthy" {
		t.Fatalf("replacement playback = %+v, status = %d, body = %s", second, response.Code, response.Body.String())
	}
	if searches.Load() != 1 {
		t.Fatalf("fresh release searches = %d, want 1", searches.Load())
	}
	if response := failHLS(second.ID); response.Code != http.StatusBadGateway {
		t.Fatalf("second HLS status = %d, body = %s", response.Code, response.Body.String())
	}

	third, response := create()
	if third != nil || response.Code != http.StatusBadGateway ||
		!strings.Contains(response.Body.String(), "unavailable after 2 release attempts") {
		t.Fatalf("terminal playback = %+v, status = %d, body = %s", third, response.Code, response.Body.String())
	}
	if searches.Load() != 1 || len(engine.created) != 2 {
		t.Fatalf("terminal retry searched %d times and mounted %d candidates", searches.Load(), len(engine.created))
	}
}

func TestHLSSubtitleProbeFailureInvalidatesTorrentRelease(t *testing.T) {
	store := playbackcache.New(t.TempDir())
	candidate := catalog.RankedCandidate{Candidate: catalog.Candidate{
		ID: "stalled-release", Indexer: "torrentleech", Name: "Show.S03.1080p",
		Protocol: catalog.ProtocolTorrent,
	}}
	if _, err := store.Save(
		"tmdb-tv:1:s3", "Show", 2008, candidate, []byte("metainfo"),
	); err != nil {
		t.Fatal(err)
	}
	engine := &fakeTorrentPlaybackEngine{sessions: map[string]*torrentstream.Session{
		"stalled-playback": {ID: "stalled-playback"},
	}}
	server := &Server{
		engine:        engine,
		playbackCache: store,
		hlsManager: &fakeHLSManager{
			probeErrors: map[string]error{"stalled-playback": errors.New("probe playback media: context deadline exceeded")},
		},
		playbackCacheKeys: map[string]playbackCacheKey{
			"stalled-playback": {
				mediaID: "tmdb-tv:1:s3", title: "Show", year: 2008,
				source: catalog.ProtocolTorrent,
			},
		},
		selected: map[string]catalog.RankedCandidate{"stalled-playback": candidate},
		playbackRequests: map[string]CreatePlaybackRequest{
			"stalled-playback": {SeasonNumber: 3, EpisodeNumber: 7},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/playbacks/stalled-playback/hls/subtitles", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("subtitle probe status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, found, err := store.Lookup("tmdb-tv:1:s3", "Show", 2008); err != nil || found {
		t.Fatalf("cached torrent playback found = %v, error = %v", found, err)
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
	manager := &fakeHLSManager{
		dir: dir,
		subtitleTracks: map[string][]hls.SubtitleTrack{
			"abc": {{Index: 6, Language: "en", Codec: "hdmv_pgs_subtitle", Kind: "bitmap"}},
		},
	}
	server := &Server{
		hlsManager: manager,
		usenetEngine: &fakeUsenetPlaybackEngine{
			session: &usenetstream.Session{ID: "abc"},
		},
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/playbacks/abc/hls/subtitles", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"kind":"bitmap"`) {
		t.Fatalf("subtitle list status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodPost, "/v1/playbacks/abc/hls",
		strings.NewReader(`{"start_seconds":12,"bitmap_subtitle_index":6}`),
	)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || manager.bitmapSubtitle != 6 ||
		!strings.Contains(response.Body.String(), `"burned_subtitle_index":6`) {
		t.Fatalf("HLS start status = %d, bitmap = %d, body = %s", response.Code, manager.bitmapSubtitle, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/playbacks/abc/hls/subtitles/6", nil)
	response = httptest.NewRecorder()
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
