package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pgeske/filmstream/internal/catalog"
)

type InternetArchive struct {
	name     string
	endpoint string
	client   *http.Client
}

func NewInternetArchive(name, endpoint string, client *http.Client) *InternetArchive {
	return &InternetArchive{name: name, endpoint: strings.TrimRight(endpoint, "/"), client: client}
}

func (i *InternetArchive) Name() string { return i.name }

func (i *InternetArchive) Search(ctx context.Context, request catalog.SearchRequest) ([]catalog.Candidate, error) {
	query := fmt.Sprintf(`mediatype:(movies) AND title:(%q)`, request.Query)
	if request.Year > 0 {
		query += fmt.Sprintf(" AND year:%d", request.Year)
	}
	parameters := url.Values{
		"q":      {query},
		"fl[]":   {"identifier", "title", "year", "language", "downloads"},
		"rows":   {"25"},
		"page":   {"1"},
		"output": {"json"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.endpoint+"/advancedsearch.php?"+parameters.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "filmstream/0.1")
	response, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s search: %w", i.name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s search returned %s", i.name, response.Status)
	}

	var payload struct {
		Response struct {
			Docs []struct {
				Identifier string      `json:"identifier"`
				Title      string      `json:"title"`
				Year       flexibleInt `json:"year"`
				Language   stringList  `json:"language"`
				Downloads  int64       `json:"downloads"`
			} `json:"docs"`
		} `json:"response"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", i.name, err)
	}

	candidates := make([]catalog.Candidate, 0, len(payload.Response.Docs))
	for _, doc := range payload.Response.Docs {
		name := doc.Title
		if name == "" {
			name = doc.Identifier
		}
		candidates = append(candidates, catalog.Candidate{
			Protocol:   catalog.ProtocolTorrent,
			ID:         doc.Identifier,
			Name:       name,
			Year:       int(doc.Year),
			Languages:  []string(doc.Language),
			Popularity: doc.Downloads,
		})
	}
	return candidates, nil
}

func (i *InternetArchive) Resolve(ctx context.Context, candidate catalog.Candidate) (Source, error) {
	metadataURL, err := url.JoinPath(i.endpoint, "metadata", candidate.ID)
	if err != nil {
		return Source{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return Source{}, err
	}
	req.Header.Set("User-Agent", "filmstream/0.1")
	response, err := i.client.Do(req)
	if err != nil {
		return Source{}, fmt.Errorf("resolve %s item %q: %w", i.name, candidate.ID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Source{}, fmt.Errorf("resolve %s item %q returned %s", i.name, candidate.ID, response.Status)
	}

	var payload struct {
		Files []struct {
			Name   string `json:"name"`
			Format string `json:"format"`
		} `json:"files"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Source{}, fmt.Errorf("decode %s metadata: %w", i.name, err)
	}

	var torrentName string
	for _, file := range payload.Files {
		if file.Format == "Archive BitTorrent" || strings.HasSuffix(strings.ToLower(file.Name), ".torrent") {
			torrentName = file.Name
			if strings.HasSuffix(file.Name, "_archive.torrent") {
				break
			}
		}
	}
	if torrentName == "" {
		return Source{}, fmt.Errorf("%s item %q has no torrent file", i.name, candidate.ID)
	}
	torrentURL, err := url.JoinPath(i.endpoint, "download", candidate.ID, torrentName)
	if err != nil {
		return Source{}, err
	}
	return Source{TorrentURL: torrentURL}, nil
}

type flexibleInt int

func (i *flexibleInt) UnmarshalJSON(value []byte) error {
	var number int
	if err := json.Unmarshal(value, &number); err == nil {
		*i = flexibleInt(number)
		return nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return err
	}
	parsed, err := strconv.Atoi(text)
	if err != nil {
		return nil
	}
	*i = flexibleInt(parsed)
	return nil
}

type stringList []string

func (s *stringList) UnmarshalJSON(value []byte) error {
	var list []string
	if err := json.Unmarshal(value, &list); err == nil {
		*s = list
		return nil
	}
	var single string
	if err := json.Unmarshal(value, &single); err != nil {
		return err
	}
	*s = []string{single}
	return nil
}
