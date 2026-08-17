package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxTMDBResponseBytes = 4 << 20
	posterBaseURL        = "https://image.tmdb.org/t/p/w500"
	backdropBaseURL      = "https://image.tmdb.org/t/p/w1280"
	stillBaseURL         = "https://image.tmdb.org/t/p/w780"
)

var tmdbMovieGenres = map[int]string{
	12: "Adventure", 14: "Fantasy", 16: "Animation", 18: "Drama", 27: "Horror",
	28: "Action", 35: "Comedy", 36: "History", 37: "Western", 53: "Thriller",
	80: "Crime", 99: "Documentary", 878: "Science Fiction", 9648: "Mystery",
	10402: "Music", 10749: "Romance", 10751: "Family", 10752: "War", 10770: "TV Movie",
}

var tmdbTVGenres = map[int]string{
	16: "Animation", 18: "Drama", 35: "Comedy", 37: "Western", 80: "Crime",
	99: "Documentary", 9648: "Mystery", 10751: "Family", 10759: "Action & Adventure",
	10762: "Kids", 10763: "News", 10764: "Reality", 10765: "Sci-Fi & Fantasy",
	10766: "Soap", 10767: "Talk", 10768: "War & Politics",
}

type TMDB struct {
	baseURL  string
	token    string
	language string
	client   *http.Client

	mu      sync.RWMutex
	imdbIDs map[string]string
	shows   map[string]Show
	seasons map[string]Season
}

type tmdbMediaResult struct {
	ID               int64   `json:"id"`
	MediaType        string  `json:"media_type"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"original_title"`
	OriginalLanguage string  `json:"original_language"`
	ReleaseDate      string  `json:"release_date"`
	Name             string  `json:"name"`
	OriginalName     string  `json:"original_name"`
	FirstAirDate     string  `json:"first_air_date"`
	Overview         string  `json:"overview"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	VoteAverage      float64 `json:"vote_average"`
	VoteCount        int     `json:"vote_count"`
	Popularity       float64 `json:"popularity"`
	GenreIDs         []int   `json:"genre_ids"`
	Adult            bool    `json:"adult"`
}

func NewTMDB(baseURL, token, language string, client *http.Client) (*TMDB, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse TMDB base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("TMDB base URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("TMDB base URL must include a host")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("TMDB API token cannot be empty")
	}
	if language == "" {
		language = "en-US"
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &TMDB{
		baseURL: baseURL, token: strings.TrimSpace(token), language: language, client: client,
		imdbIDs: make(map[string]string), shows: make(map[string]Show), seasons: make(map[string]Season),
	}, nil
}

func (t *TMDB) Search(ctx context.Context, query string) ([]Movie, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("media search cannot be empty")
	}
	values := url.Values{
		"query": {query}, "include_adult": {"false"}, "language": {t.language}, "page": {"1"},
	}
	items, err := t.fetchMedia(ctx, "search/multi", values, "")
	if err != nil {
		return nil, err
	}
	return t.enrichShowSummaries(ctx, items), nil
}

func (t *TMDB) IMDbID(ctx context.Context, mediaID string) (string, error) {
	kind, id, err := parseTMDBMediaID(mediaID)
	if err != nil {
		return "", err
	}

	t.mu.RLock()
	cached, ok := t.imdbIDs[mediaID]
	t.mu.RUnlock()
	if ok {
		return cached, nil
	}

	var payload struct {
		IMDbID string `json:"imdb_id"`
	}
	if err := t.getJSON(ctx, kind+"/"+id+"/external_ids", nil, &payload); err != nil {
		return "", fmt.Errorf("request TMDB external IDs: %w", err)
	}
	imdbID := strings.TrimSpace(payload.IMDbID)
	if !strings.HasPrefix(imdbID, "tt") {
		return "", fmt.Errorf("TMDB %s %s has no IMDb ID", kind, id)
	}
	if _, err := strconv.ParseUint(strings.TrimPrefix(imdbID, "tt"), 10, 64); err != nil {
		return "", fmt.Errorf("TMDB returned invalid IMDb ID %q", imdbID)
	}

	t.mu.Lock()
	t.imdbIDs[mediaID] = imdbID
	t.mu.Unlock()
	return imdbID, nil
}

func (t *TMDB) Discover(ctx context.Context, collection Collection) ([]Movie, error) {
	movieValues := url.Values{
		"include_adult": {"false"}, "include_video": {"false"}, "language": {t.language},
		"page": {"1"}, "release_date.lte": {time.Now().UTC().Format(time.DateOnly)},
		"with_release_type": {"4|5|6"},
	}
	tvValues := url.Values{
		"include_adult": {"false"}, "language": {t.language}, "page": {"1"},
		"first_air_date.lte": {time.Now().UTC().Format(time.DateOnly)},
	}
	switch collection {
	case CollectionPopular:
		movieValues.Set("sort_by", "popularity.desc")
		tvValues.Set("sort_by", "popularity.desc")
	case CollectionTopRated:
		movieValues.Set("sort_by", "vote_average.desc")
		movieValues.Set("vote_count.gte", "5000")
		tvValues.Set("sort_by", "vote_average.desc")
		tvValues.Set("vote_count.gte", "2000")
	default:
		return nil, fmt.Errorf("unsupported media collection %q", collection)
	}

	type result struct {
		items []Movie
		err   error
	}
	results := make(chan result, 2)
	go func() {
		items, err := t.fetchMedia(ctx, "discover/movie", movieValues, "movie")
		results <- result{items: items, err: err}
	}()
	go func() {
		items, err := t.fetchMedia(ctx, "discover/tv", tvValues, "tv")
		results <- result{items: items, err: err}
	}()

	var items []Movie
	var failures []error
	for range 2 {
		result := <-results
		items = append(items, result.items...)
		if result.err != nil {
			failures = append(failures, result.err)
		}
	}
	if len(items) == 0 && len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if collection == CollectionPopular {
			return items[i].Popularity > items[j].Popularity
		}
		if items[i].VoteAverage == items[j].VoteAverage {
			return items[i].VoteCount > items[j].VoteCount
		}
		return items[i].VoteAverage > items[j].VoteAverage
	})
	items = interleaveMediaTypes(items, 20)
	return t.enrichShowSummaries(ctx, items), nil
}

func (t *TMDB) Show(ctx context.Context, mediaID string) (Show, error) {
	kind, id, err := parseTMDBMediaID(mediaID)
	if err != nil {
		return Show{}, err
	}
	if kind != "tv" {
		return Show{}, fmt.Errorf("media ID %q is not a show", mediaID)
	}

	t.mu.RLock()
	cached, ok := t.shows[mediaID]
	t.mu.RUnlock()
	if ok {
		return cached, nil
	}

	var payload struct {
		ID               int64   `json:"id"`
		Name             string  `json:"name"`
		OriginalName     string  `json:"original_name"`
		OriginalLanguage string  `json:"original_language"`
		FirstAirDate     string  `json:"first_air_date"`
		Overview         string  `json:"overview"`
		PosterPath       string  `json:"poster_path"`
		BackdropPath     string  `json:"backdrop_path"`
		VoteAverage      float64 `json:"vote_average"`
		VoteCount        int     `json:"vote_count"`
		Popularity       float64 `json:"popularity"`
		NumberOfSeasons  int     `json:"number_of_seasons"`
		Genres           []struct {
			Name string `json:"name"`
		} `json:"genres"`
		Seasons []struct {
			AirDate      string `json:"air_date"`
			EpisodeCount int    `json:"episode_count"`
			Name         string `json:"name"`
			PosterPath   string `json:"poster_path"`
			SeasonNumber int    `json:"season_number"`
		} `json:"seasons"`
	}
	if err := t.getJSON(ctx, "tv/"+id, url.Values{"language": {t.language}}, &payload); err != nil {
		return Show{}, err
	}
	show := Show{Movie: Movie{
		ID: mediaID, MediaType: MediaTypeShow, Title: strings.TrimSpace(payload.Name),
		OriginalTitle:    strings.TrimSpace(payload.OriginalName),
		OriginalLanguage: strings.TrimSpace(payload.OriginalLanguage), Year: releaseYear(payload.FirstAirDate),
		ReleaseDate: payload.FirstAirDate, Overview: strings.TrimSpace(payload.Overview),
		VoteAverage: payload.VoteAverage, VoteCount: payload.VoteCount, Popularity: payload.Popularity,
		NumberOfSeasons: payload.NumberOfSeasons,
	}}
	if payload.PosterPath != "" {
		show.PosterURL = posterBaseURL + payload.PosterPath
	}
	if payload.BackdropPath != "" {
		show.BackdropURL = backdropBaseURL + payload.BackdropPath
	}
	for _, genre := range payload.Genres {
		if name := strings.TrimSpace(genre.Name); name != "" {
			show.Genres = append(show.Genres, name)
		}
	}
	today := time.Now().UTC().Format(time.DateOnly)
	hiddenFutureSeasons := 0
	for _, season := range payload.Seasons {
		if season.SeasonNumber <= 0 || season.EpisodeCount <= 0 {
			continue
		}
		if season.AirDate != "" && season.AirDate > today {
			hiddenFutureSeasons++
			continue
		}
		summary := SeasonSummary{
			Number: season.SeasonNumber, Name: strings.TrimSpace(season.Name),
			EpisodeCount: season.EpisodeCount, AirDate: season.AirDate,
		}
		if summary.Name == "" {
			summary.Name = fmt.Sprintf("Season %d", summary.Number)
		}
		if season.PosterPath != "" {
			summary.PosterURL = posterBaseURL + season.PosterPath
		}
		show.Seasons = append(show.Seasons, summary)
	}
	sort.Slice(show.Seasons, func(i, j int) bool { return show.Seasons[i].Number < show.Seasons[j].Number })
	show.NumberOfSeasons = max(len(show.Seasons), show.NumberOfSeasons-hiddenFutureSeasons)

	t.mu.Lock()
	t.shows[mediaID] = show
	t.mu.Unlock()
	return show, nil
}

func (t *TMDB) Season(ctx context.Context, mediaID string, number int) (Season, error) {
	if number <= 0 {
		return Season{}, errors.New("season number must be positive")
	}
	_, id, err := parseTMDBMediaID(mediaID)
	if err != nil || !strings.HasPrefix(mediaID, "tmdb-tv:") {
		return Season{}, fmt.Errorf("invalid TMDB show ID %q", mediaID)
	}
	cacheKey := fmt.Sprintf("%s/%d", mediaID, number)
	t.mu.RLock()
	cached, ok := t.seasons[cacheKey]
	t.mu.RUnlock()
	if ok {
		return cached, nil
	}

	show, err := t.Show(ctx, mediaID)
	if err != nil {
		return Season{}, err
	}
	var payload struct {
		Name         string `json:"name"`
		Overview     string `json:"overview"`
		PosterPath   string `json:"poster_path"`
		SeasonNumber int    `json:"season_number"`
		Episodes     []struct {
			ID            int64   `json:"id"`
			Name          string  `json:"name"`
			Overview      string  `json:"overview"`
			AirDate       string  `json:"air_date"`
			StillPath     string  `json:"still_path"`
			VoteAverage   float64 `json:"vote_average"`
			Runtime       int     `json:"runtime"`
			SeasonNumber  int     `json:"season_number"`
			EpisodeNumber int     `json:"episode_number"`
		} `json:"episodes"`
	}
	path := fmt.Sprintf("tv/%s/season/%d", id, number)
	if err := t.getJSON(ctx, path, url.Values{"language": {t.language}}, &payload); err != nil {
		return Season{}, err
	}
	season := Season{
		SeriesID: mediaID, SeriesTitle: show.Title, Number: number,
		Name: strings.TrimSpace(payload.Name), Overview: strings.TrimSpace(payload.Overview),
	}
	if season.Name == "" {
		season.Name = fmt.Sprintf("Season %d", number)
	}
	if payload.PosterPath != "" {
		season.PosterURL = posterBaseURL + payload.PosterPath
	}
	today := time.Now().UTC().Format(time.DateOnly)
	for _, item := range payload.Episodes {
		if item.AirDate != "" && item.AirDate > today {
			continue
		}
		seasonNumber := item.SeasonNumber
		if seasonNumber <= 0 {
			seasonNumber = number
		}
		if item.EpisodeNumber <= 0 || strings.TrimSpace(item.Name) == "" {
			continue
		}
		episode := Episode{
			ID:       fmt.Sprintf("%s:s%d:e%d", mediaID, seasonNumber, item.EpisodeNumber),
			SeriesID: mediaID, SeriesTitle: show.Title, SeasonNumber: seasonNumber,
			EpisodeNumber: item.EpisodeNumber, Title: strings.TrimSpace(item.Name),
			Overview: strings.TrimSpace(item.Overview), AirDate: item.AirDate,
			VoteAverage: item.VoteAverage, Runtime: item.Runtime,
		}
		if item.StillPath != "" {
			episode.StillURL = stillBaseURL + item.StillPath
		}
		season.Episodes = append(season.Episodes, episode)
	}
	sort.Slice(season.Episodes, func(i, j int) bool {
		return season.Episodes[i].EpisodeNumber < season.Episodes[j].EpisodeNumber
	})

	t.mu.Lock()
	t.seasons[cacheKey] = season
	t.mu.Unlock()
	return season, nil
}

func interleaveMediaTypes(items []Movie, limit int) []Movie {
	if limit <= 0 {
		return nil
	}
	movies := make([]Movie, 0, len(items))
	shows := make([]Movie, 0, len(items))
	for _, item := range items {
		if item.MediaType == MediaTypeShow {
			shows = append(shows, item)
		} else {
			movies = append(movies, item)
		}
	}
	if len(movies) == 0 || len(shows) == 0 {
		return items[:min(limit, len(items))]
	}

	result := make([]Movie, 0, min(limit, len(items)))
	movieIndex, showIndex := 0, 0
	preferShow := items[0].MediaType == MediaTypeShow
	for len(result) < limit && (movieIndex < len(movies) || showIndex < len(shows)) {
		if preferShow && showIndex < len(shows) {
			result = append(result, shows[showIndex])
			showIndex++
		} else if !preferShow && movieIndex < len(movies) {
			result = append(result, movies[movieIndex])
			movieIndex++
		} else if showIndex < len(shows) {
			result = append(result, shows[showIndex])
			showIndex++
		} else {
			result = append(result, movies[movieIndex])
			movieIndex++
		}
		preferShow = !preferShow
	}
	return result
}

func (t *TMDB) enrichShowSummaries(ctx context.Context, items []Movie) []Movie {
	type result struct {
		index int
		show  Show
	}
	results := make(chan result, len(items))
	count := 0
	for index, item := range items {
		if item.MediaType != MediaTypeShow || item.NumberOfSeasons > 0 {
			continue
		}
		count++
		go func(index int, id string) {
			show, _ := t.Show(ctx, id)
			results <- result{index: index, show: show}
		}(index, item.ID)
	}
	for range count {
		result := <-results
		if result.show.ID == "" {
			continue
		}
		items[result.index].NumberOfSeasons = result.show.NumberOfSeasons
		if len(items[result.index].Genres) == 0 {
			items[result.index].Genres = result.show.Genres
		}
	}
	return items
}

func (t *TMDB) fetchMedia(ctx context.Context, path string, values url.Values, forcedType string) ([]Movie, error) {
	var payload struct {
		Results []tmdbMediaResult `json:"results"`
	}
	if err := t.getJSON(ctx, path, values, &payload); err != nil {
		return nil, err
	}
	items := make([]Movie, 0, len(payload.Results))
	for _, result := range payload.Results {
		mediaType := result.MediaType
		if mediaType == "" {
			mediaType = forcedType
		}
		if mediaType != "movie" && mediaType != "tv" {
			continue
		}
		title := result.Title
		originalTitle := result.OriginalTitle
		releaseDate := result.ReleaseDate
		itemType := MediaTypeMovie
		idPrefix := "tmdb:"
		genreMap := tmdbMovieGenres
		if mediaType == "tv" {
			title = result.Name
			originalTitle = result.OriginalName
			releaseDate = result.FirstAirDate
			itemType = MediaTypeShow
			idPrefix = "tmdb-tv:"
			genreMap = tmdbTVGenres
		}
		title = strings.TrimSpace(title)
		if result.Adult || result.ID <= 0 || title == "" {
			continue
		}
		item := Movie{
			ID: idPrefix + strconv.FormatInt(result.ID, 10), MediaType: itemType,
			Title: title, OriginalTitle: strings.TrimSpace(originalTitle),
			OriginalLanguage: strings.TrimSpace(result.OriginalLanguage),
			Year:             releaseYear(releaseDate), ReleaseDate: releaseDate,
			Overview: strings.TrimSpace(result.Overview), VoteAverage: result.VoteAverage,
			VoteCount: result.VoteCount, Popularity: result.Popularity,
			Genres: genreNames(result.GenreIDs, genreMap),
		}
		if result.PosterPath != "" {
			item.PosterURL = posterBaseURL + result.PosterPath
		}
		if result.BackdropPath != "" {
			item.BackdropURL = backdropBaseURL + result.BackdropPath
		}
		items = append(items, item)
	}
	return items, nil
}

func (t *TMDB) getJSON(ctx context.Context, path string, values url.Values, destination any) error {
	endpoint, err := url.JoinPath(t.baseURL, path)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if values != nil {
		parsed.RawQuery = values.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+t.token)
	response, err := t.client.Do(request)
	if err != nil {
		return fmt.Errorf("request TMDB: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTMDBResponseBytes))
	if err != nil {
		return fmt.Errorf("read TMDB response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			StatusMessage string `json:"status_message"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.StatusMessage != "" {
			return fmt.Errorf("TMDB returned %s: %s", response.Status, payload.StatusMessage)
		}
		return fmt.Errorf("TMDB returned %s", response.Status)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("decode TMDB response: %w", err)
	}
	return nil
}

func parseTMDBMediaID(mediaID string) (kind string, id string, err error) {
	mediaID = strings.TrimSpace(mediaID)
	switch {
	case strings.HasPrefix(mediaID, "tmdb-tv:"):
		kind, id = "tv", strings.TrimPrefix(mediaID, "tmdb-tv:")
	case strings.HasPrefix(mediaID, "tmdb:"):
		kind, id = "movie", strings.TrimPrefix(mediaID, "tmdb:")
	default:
		return "", "", fmt.Errorf("unsupported TMDB media ID %q", mediaID)
	}
	if value, parseErr := strconv.ParseInt(id, 10, 64); parseErr != nil || value <= 0 {
		return "", "", fmt.Errorf("invalid TMDB %s ID %q", kind, mediaID)
	}
	return kind, id, nil
}

func genreNames(ids []int, names map[int]string) []string {
	genres := make([]string, 0, len(ids))
	for _, id := range ids {
		if genre, ok := names[id]; ok {
			genres = append(genres, genre)
		}
	}
	return genres
}

func releaseYear(releaseDate string) int {
	if len(releaseDate) < 4 {
		return 0
	}
	year, err := strconv.Atoi(releaseDate[:4])
	if err != nil || year < 1888 || year > 2100 {
		return 0
	}
	return year
}
