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
	"sync"
)

const maxOMDbResponseBytes = 1 << 20

type ratingsUnavailableError struct {
	message string
}

func (e ratingsUnavailableError) Error() string {
	return e.message
}

func (e ratingsUnavailableError) Is(target error) bool {
	return target == ErrRatingsUnavailable
}

type OMDb struct {
	baseURL string
	apiKey  string
	client  *http.Client
	mu      sync.RWMutex
	cache   map[string]MovieRatings
}

func NewOMDb(baseURL, apiKey string, client *http.Client) (*OMDb, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse OMDb base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("OMDb base URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("OMDb base URL must include a host")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("OMDb API key cannot be empty")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &OMDb{baseURL: baseURL, apiKey: apiKey, client: client, cache: make(map[string]MovieRatings)}, nil
}

func (o *OMDb) Ratings(ctx context.Context, title string, year int) (MovieRatings, error) {
	return o.ratingsByTitle(ctx, title, year, "movie")
}

func (o *OMDb) RatingsForMedia(ctx context.Context, item Movie) (MovieRatings, error) {
	mediaType := "movie"
	if item.MediaType == MediaTypeShow {
		mediaType = "series"
	}
	return o.ratingsByTitle(ctx, item.Title, item.Year, mediaType)
}

func (o *OMDb) ratingsByTitle(ctx context.Context, title string, year int, mediaType string) (MovieRatings, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return MovieRatings{}, errors.New("rating title cannot be empty")
	}
	values := make(url.Values)
	values.Set("t", title)
	values.Set("type", mediaType)
	if year > 0 {
		values.Set("y", strconv.Itoa(year))
	}
	cacheKey := "title\x00" + mediaType + "\x00" + strings.ToLower(title) + "\x00" + strconv.Itoa(year)
	return o.fetchRatings(ctx, cacheKey, values)
}

func (o *OMDb) RatingsByIMDbID(ctx context.Context, imdbID string) (MovieRatings, error) {
	imdbID = strings.TrimSpace(imdbID)
	if !strings.HasPrefix(imdbID, "tt") {
		return MovieRatings{}, errors.New("invalid IMDb ID")
	}
	if _, err := strconv.ParseUint(strings.TrimPrefix(imdbID, "tt"), 10, 64); err != nil {
		return MovieRatings{}, errors.New("invalid IMDb ID")
	}
	values := make(url.Values)
	values.Set("i", imdbID)
	values.Set("type", "movie")
	return o.fetchRatings(ctx, "imdb\x00"+imdbID, values)
}

func (o *OMDb) fetchRatings(ctx context.Context, cacheKey string, values url.Values) (MovieRatings, error) {
	o.mu.RLock()
	cached, ok := o.cache[cacheKey]
	o.mu.RUnlock()
	if ok {
		return cached, nil
	}

	parsed, err := url.Parse(o.baseURL)
	if err != nil {
		return MovieRatings{}, err
	}
	query := parsed.Query()
	for name, entries := range values {
		query[name] = entries
	}
	query.Set("apikey", o.apiKey)
	parsed.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return MovieRatings{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := o.client.Do(request)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return MovieRatings{}, ratingsUnavailableError{message: fmt.Sprintf("request OMDb: %v", err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOMDbResponseBytes))
	if err != nil {
		return MovieRatings{}, fmt.Errorf("read OMDb response: %w", err)
	}
	var payload struct {
		Response   string `json:"Response"`
		Error      string `json:"Error"`
		IMDbRating string `json:"imdbRating"`
		IMDbVotes  string `json:"imdbVotes"`
		Rated      string `json:"Rated"`
		Ratings    []struct {
			Source string `json:"Source"`
			Value  string `json:"Value"`
		} `json:"Ratings"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return MovieRatings{}, fmt.Errorf("decode OMDb response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || payload.Response != "True" {
		message := "OMDb returned " + response.Status
		if payload.Error != "" {
			message = "OMDb: " + payload.Error
		}
		lowerMessage := strings.ToLower(message)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError || strings.Contains(lowerMessage, "api key") || strings.Contains(lowerMessage, "request limit") {
			return MovieRatings{}, ratingsUnavailableError{message: message}
		}
		return MovieRatings{}, errors.New(message)
	}

	ratings := MovieRatings{}
	if contentRating := strings.TrimSpace(payload.Rated); contentRating != "" && !strings.EqualFold(contentRating, "N/A") {
		ratings.ContentRating = &contentRating
	}
	if value, err := strconv.ParseFloat(payload.IMDbRating, 64); err == nil && value > 0 {
		ratings.IMDb = &value
	}
	if value, err := strconv.Atoi(strings.ReplaceAll(payload.IMDbVotes, ",", "")); err == nil && value > 0 {
		ratings.IMDbVotes = &value
	}
	for _, rating := range payload.Ratings {
		if rating.Source != "Rotten Tomatoes" {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rating.Value), "%"))
		if err == nil && value >= 0 && value <= 100 {
			ratings.RottenTomatoes = &value
		}
		break
	}

	o.mu.Lock()
	o.cache[cacheKey] = ratings
	o.mu.Unlock()
	return ratings, nil
}
