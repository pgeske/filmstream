package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/pgeske/filmstream/internal/catalog"
)

type HTTP struct {
	name     string
	endpoint string
	headers  map[string]string
	client   *http.Client
}

func NewHTTP(name, endpoint string, headers map[string]string, client *http.Client) *HTTP {
	return &HTTP{name: name, endpoint: endpoint, headers: headers, client: client}
}

func (h *HTTP) Name() string { return h.name }

func (h *HTTP) Search(ctx context.Context, request catalog.SearchRequest) ([]catalog.Candidate, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for name, value := range h.headers {
		req.Header.Set(name, value)
	}

	response, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s search: %w", h.name, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s search returned %s", h.name, response.Status)
	}
	var payload struct {
		Candidates []catalog.Candidate `json:"candidates"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", h.name, err)
	}
	return payload.Candidates, nil
}

func (h *HTTP) Resolve(_ context.Context, candidate catalog.Candidate) (Source, error) {
	if candidate.MagnetURI == "" && candidate.TorrentURL == "" {
		return Source{}, errors.New("HTTP indexer candidate has no magnet_uri or torrent_url")
	}
	return Source{MagnetURI: candidate.MagnetURI, TorrentURL: candidate.TorrentURL}, nil
}
