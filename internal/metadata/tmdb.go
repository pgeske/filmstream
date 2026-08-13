package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxTMDBResponseBytes = 4 << 20
	posterBaseURL        = "https://image.tmdb.org/t/p/w500"
	backdropBaseURL      = "https://image.tmdb.org/t/p/w1280"
)

type TMDB struct {
	baseURL  string
	token    string
	language string
	client   *http.Client
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
	return &TMDB{baseURL: baseURL, token: strings.TrimSpace(token), language: language, client: client}, nil
}

func (t *TMDB) Search(ctx context.Context, query string) ([]Movie, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("movie search cannot be empty")
	}
	values := make(url.Values)
	values.Set("query", query)
	values.Set("include_adult", "false")
	values.Set("language", t.language)
	values.Set("page", "1")
	return t.fetchMovies(ctx, "search/movie", values)
}

func (t *TMDB) Discover(ctx context.Context, collection Collection) ([]Movie, error) {
	values := make(url.Values)
	values.Set("include_adult", "false")
	values.Set("include_video", "false")
	values.Set("language", t.language)
	values.Set("page", "1")
	values.Set("release_date.lte", time.Now().UTC().Format(time.DateOnly))
	values.Set("with_release_type", "4|5|6")
	switch collection {
	case CollectionPopular:
		values.Set("sort_by", "popularity.desc")
	case CollectionTopRated:
		values.Set("sort_by", "vote_average.desc")
		values.Set("vote_count.gte", "1000")
	default:
		return nil, fmt.Errorf("unsupported movie collection %q", collection)
	}
	return t.fetchMovies(ctx, "discover/movie", values)
}

func (t *TMDB) fetchMovies(ctx context.Context, path string, values url.Values) ([]Movie, error) {
	endpoint, err := url.JoinPath(t.baseURL, path)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	parsed.RawQuery = values.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+t.token)
	response, err := t.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request TMDB: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTMDBResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read TMDB response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			StatusMessage string `json:"status_message"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.StatusMessage != "" {
			return nil, fmt.Errorf("TMDB returned %s: %s", response.Status, payload.StatusMessage)
		}
		return nil, fmt.Errorf("TMDB returned %s", response.Status)
	}
	var payload struct {
		Results []struct {
			ID            int64   `json:"id"`
			Title         string  `json:"title"`
			OriginalTitle string  `json:"original_title"`
			ReleaseDate   string  `json:"release_date"`
			Overview      string  `json:"overview"`
			PosterPath    string  `json:"poster_path"`
			BackdropPath  string  `json:"backdrop_path"`
			VoteAverage   float64 `json:"vote_average"`
			Adult         bool    `json:"adult"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode TMDB response: %w", err)
	}
	movies := make([]Movie, 0, len(payload.Results))
	for _, result := range payload.Results {
		result.Title = strings.TrimSpace(result.Title)
		if result.Adult || result.ID <= 0 || result.Title == "" {
			continue
		}
		movie := Movie{
			ID:            "tmdb:" + strconv.FormatInt(result.ID, 10),
			Title:         result.Title,
			OriginalTitle: strings.TrimSpace(result.OriginalTitle),
			Year:          releaseYear(result.ReleaseDate),
			ReleaseDate:   result.ReleaseDate,
			Overview:      strings.TrimSpace(result.Overview),
			VoteAverage:   result.VoteAverage,
		}
		if result.PosterPath != "" {
			movie.PosterURL = posterBaseURL + result.PosterPath
		}
		if result.BackdropPath != "" {
			movie.BackdropURL = backdropBaseURL + result.BackdropPath
		}
		movies = append(movies, movie)
	}
	return movies, nil
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
