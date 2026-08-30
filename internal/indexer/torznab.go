package indexer

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/pgeske/filmstream/internal/catalog"
)

const maxTorznabResponseBytes = 16 << 20

type Capabilities struct {
	SearchAvailable      bool
	MovieSearchAvailable bool
	MovieSearchParams    map[string]bool
	TVSearchAvailable    bool
	TVSearchParams       map[string]bool
}

type Torznab struct {
	name     string
	endpoint *url.URL
	apiKey   string
	client   *http.Client

	capabilitiesMu sync.Mutex
	capabilities   *Capabilities
}

func NewTorznab(name, endpoint, apiKey string, client *http.Client) (*Torznab, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse Torznab endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("Torznab endpoint must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("Torznab endpoint must include a host")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("Torznab endpoint cannot include a fragment")
	}
	if parsed.Query().Get("apikey") != "" {
		return nil, errors.New("remove apikey from the endpoint and enter it at the secure prompt")
	}
	return &Torznab{name: name, endpoint: parsed, apiKey: apiKey, client: client}, nil
}

func (t *Torznab) Name() string { return t.name }

func (t *Torznab) Capabilities(ctx context.Context) (Capabilities, error) {
	t.capabilitiesMu.Lock()
	defer t.capabilitiesMu.Unlock()
	if t.capabilities != nil {
		return *t.capabilities, nil
	}

	requestURL := t.requestURL(map[string]string{"t": "caps"})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return Capabilities{}, err
	}
	response, err := t.client.Do(request)
	if err != nil {
		return Capabilities{}, fmt.Errorf("query %s capabilities: %w", t.name, requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Capabilities{}, fmt.Errorf("query %s capabilities returned %s", t.name, response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxTorznabResponseBytes))
	if err != nil {
		return Capabilities{}, fmt.Errorf("read %s capabilities: %w", t.name, err)
	}
	if err := torznabResponseError(body); err != nil {
		return Capabilities{}, fmt.Errorf("query %s capabilities: %w", t.name, err)
	}
	var payload struct {
		Searching struct {
			Search struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"search"`
			Movie struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"movie-search"`
			TV struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"tv-search"`
		} `xml:"searching"`
	}
	if err := xml.Unmarshal(body, &payload); err != nil {
		return Capabilities{}, fmt.Errorf("decode %s capabilities: %w", t.name, err)
	}
	capabilities := Capabilities{
		SearchAvailable:      available(payload.Searching.Search.Available),
		MovieSearchAvailable: available(payload.Searching.Movie.Available),
		MovieSearchParams:    parameterSet(payload.Searching.Movie.SupportedParams),
		TVSearchAvailable:    available(payload.Searching.TV.Available),
		TVSearchParams:       parameterSet(payload.Searching.TV.SupportedParams),
	}
	if !capabilities.SearchAvailable && !capabilities.MovieSearchAvailable && !capabilities.TVSearchAvailable {
		return Capabilities{}, fmt.Errorf("%s supports no compatible searches", t.name)
	}
	t.capabilities = &capabilities
	return capabilities, nil
}

func (t *Torznab) Search(ctx context.Context, request catalog.SearchRequest) ([]catalog.Candidate, error) {
	capabilities, err := t.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if request.MediaType == "show" && request.SeasonNumber > 0 && request.EpisodeNumber > 0 {
		seasonQuery := fmt.Sprintf("%s S%02d", torznabQuery(request.Query), request.SeasonNumber)
		episodeQuery := fmt.Sprintf("%s S%02dE%02d", torznabQuery(request.Query), request.SeasonNumber, request.EpisodeNumber)
		seasonParameters := map[string]string{"t": "search", "q": seasonQuery, "limit": "100"}
		episodeParameters := map[string]string{"t": "search", "q": episodeQuery, "limit": "100"}

		searches := []map[string]string{seasonParameters}
		if capabilities.TVSearchAvailable {
			tvParameters := map[string]string{"t": "tvsearch", "q": torznabQuery(request.Query), "limit": "100"}
			if capabilities.TVSearchParams["season"] {
				tvParameters["season"] = strconv.Itoa(request.SeasonNumber)
			} else {
				tvParameters["q"] = seasonQuery
			}
			searches = append(searches, tvParameters)
		}

		type searchResult struct {
			candidates []catalog.Candidate
			err        error
		}
		results := make(chan searchResult, len(searches))
		for _, parameters := range searches {
			go func(parameters map[string]string) {
				candidates, err := t.search(ctx, parameters)
				results <- searchResult{candidates: candidates, err: err}
			}(parameters)
		}
		var candidates []catalog.Candidate
		var failures []error
		for range searches {
			result := <-results
			if result.err != nil {
				failures = append(failures, result.err)
			} else {
				candidates = append(candidates, result.candidates...)
			}
		}
		candidates = mergeCandidates(candidates)
		if request.PreferSeasonPack {
			for _, candidate := range candidates {
				if catalog.IsSeasonPack(candidate.Name, request.SeasonNumber) {
					return candidates, nil
				}
			}
		}
		episodeCandidates, episodeErr := t.search(ctx, episodeParameters)
		if episodeErr != nil {
			failures = append(failures, episodeErr)
		} else {
			candidates = mergeCandidates(candidates, episodeCandidates)
		}
		if len(candidates) == 0 && len(failures) > 0 {
			return nil, errors.Join(failures...)
		}
		return candidates, nil
	}
	var candidates []catalog.Candidate
	var failures []error
	if capabilities.MovieSearchAvailable {
		movieParameters := map[string]string{
			"t": "movie", "q": torznabQuery(request.Query), "limit": "100",
		}
		if request.Year > 0 && capabilities.MovieSearchParams["year"] {
			movieParameters["year"] = strconv.Itoa(request.Year)
		}
		movieCandidates, movieErr := t.search(ctx, movieParameters)
		if movieErr != nil {
			failures = append(failures, movieErr)
		} else {
			candidates = mergeCandidates(candidates, movieCandidates)
			if len(catalog.Rank(request, candidates)) > 0 {
				return candidates, nil
			}
		}
	}

	if capabilities.SearchAvailable {
		for _, query := range movieFallbackQueries(request.Query, request.Year) {
			fallbackCandidates, fallbackErr := t.search(ctx, map[string]string{
				"t": "search", "q": query, "limit": "100",
			})
			if fallbackErr != nil {
				failures = append(failures, fallbackErr)
				continue
			}
			candidates = mergeCandidates(candidates, fallbackCandidates)
			if len(catalog.Rank(request, candidates)) > 0 {
				return candidates, nil
			}
		}
	}
	if len(candidates) == 0 && len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return candidates, nil
}

func (t *Torznab) search(ctx context.Context, parameters map[string]string) ([]catalog.Candidate, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, t.requestURL(parameters), nil)
	if err != nil {
		return nil, err
	}
	response, err := t.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", t.name, requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("search %s returned %s", t.name, response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxTorznabResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s search response: %w", t.name, err)
	}
	if err := torznabResponseError(body); err != nil {
		return nil, fmt.Errorf("search %s: %w", t.name, err)
	}
	var feed torznabFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("decode %s search response: %w", t.name, err)
	}
	candidates := make([]catalog.Candidate, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		candidate, ok := t.candidate(item)
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func movieFallbackQueries(title string, year int) []string {
	title = torznabQuery(title)
	queries := make([]string, 0, 2)
	if year > 0 {
		queries = append(queries, title+" "+strconv.Itoa(year))
	}
	if title != "" {
		queries = append(queries, title)
	}
	return queries
}

func torznabQuery(value string) string {
	var result strings.Builder
	spacePending := false
	for _, character := range strings.TrimSpace(value) {
		switch {
		case unicode.IsLetter(character), unicode.IsDigit(character), unicode.IsMark(character):
			if spacePending && result.Len() > 0 {
				result.WriteByte(' ')
			}
			result.WriteRune(character)
			spacePending = false
		case character == '\'', character == '’':
			// Indexer release names conventionally omit possessive apostrophes.
		default:
			spacePending = result.Len() > 0
		}
	}
	if result.Len() == 0 {
		return strings.TrimSpace(value)
	}
	return result.String()
}

func mergeCandidates(groups ...[]catalog.Candidate) []catalog.Candidate {
	seen := make(map[string]bool)
	var merged []catalog.Candidate
	for _, candidates := range groups {
		for _, candidate := range candidates {
			key := candidate.ID
			if key == "" {
				key = firstNonempty(candidate.NZBURL, candidate.TorrentURL, candidate.MagnetURI, candidate.Name)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, candidate)
		}
	}
	return merged
}

func (t *Torznab) Resolve(_ context.Context, candidate catalog.Candidate) (Source, error) {
	if candidate.MagnetURI == "" && candidate.TorrentURL == "" && candidate.NZBURL == "" {
		return Source{}, errors.New("Torznab candidate has no downloadable source")
	}
	return Source{
		Protocol: candidate.Protocol, MagnetURI: candidate.MagnetURI,
		TorrentURL: candidate.TorrentURL, NZBURL: candidate.NZBURL,
	}, nil
}

func (t *Torznab) candidate(item torznabItem) (catalog.Candidate, bool) {
	attributes := make(map[string]string, len(item.Attributes))
	var categories []int
	for _, attribute := range item.Attributes {
		name := strings.ToLower(attribute.Name)
		attributes[name] = attribute.Value
		if name == "category" {
			if category, err := strconv.Atoi(attribute.Value); err == nil {
				categories = append(categories, category)
			}
		}
	}

	download := firstNonempty(attributes["magneturl"], item.Enclosure.URL, item.Link)
	if download == "" {
		return catalog.Candidate{}, false
	}
	if !strings.HasPrefix(download, "magnet:") {
		if parsed, err := url.Parse(download); err == nil {
			download = t.endpoint.ResolveReference(parsed).String()
		}
	}

	candidate := catalog.Candidate{
		ID:           firstNonempty(item.GUID, attributes["infohash"], item.Title),
		Name:         item.Title,
		Protocol:     catalog.ProtocolTorrent,
		SizeBytes:    parseInt64(firstNonempty(attributes["size"], item.Enclosure.Length)),
		Year:         int(parseInt64(attributes["year"])),
		Categories:   categories,
		ReleaseGroup: firstNonempty(attributes["releasegroup"], attributes["group"]),
	}
	if strings.HasPrefix(download, "magnet:") {
		candidate.MagnetURI = download
	} else if strings.Contains(strings.ToLower(item.Enclosure.Type), "nzb") || strings.EqualFold(attributes["protocol"], catalog.ProtocolUsenet) {
		candidate.Protocol = catalog.ProtocolUsenet
		candidate.NZBURL = download
	} else {
		candidate.TorrentURL = download
	}
	if value, ok := parseOptionalInt(attributes["seeders"]); ok {
		candidate.Seeders = &value
	}
	if value, ok := parseOptionalInt(attributes["leechers"]); ok {
		candidate.Leechers = &value
	} else if peers, peersOK := parseOptionalInt(attributes["peers"]); peersOK && candidate.Seeders != nil {
		value = max(0, peers-*candidate.Seeders)
		candidate.Leechers = &value
	}
	if value, ok := parseOptionalFloat(attributes["downloadvolumefactor"]); ok {
		candidate.DownloadVolumeFactor = &value
	}
	if value, ok := parseOptionalFloat(attributes["uploadvolumefactor"]); ok {
		candidate.UploadVolumeFactor = &value
	}
	if language := attributes["language"]; language != "" {
		candidate.Languages = []string{language}
	}
	if published, err := mail.ParseDate(item.PublishDate); err == nil {
		candidate.PublishedUnix = published.Unix()
	}
	return candidate, candidate.Name != ""
}

func (t *Torznab) requestURL(parameters map[string]string) string {
	copy := *t.endpoint
	query := copy.Query()
	for name, value := range parameters {
		query.Set(name, value)
	}
	if t.apiKey != "" {
		query.Set("apikey", t.apiKey)
	}
	copy.RawQuery = query.Encode()
	return copy.String()
}

type torznabFeed struct {
	Channel struct {
		Items []torznabItem `xml:"item"`
	} `xml:"channel"`
}

type torznabItem struct {
	Title       string `xml:"title"`
	GUID        string `xml:"guid"`
	Link        string `xml:"link"`
	PublishDate string `xml:"pubDate"`
	Enclosure   struct {
		URL    string `xml:"url,attr"`
		Length string `xml:"length,attr"`
		Type   string `xml:"type,attr"`
	} `xml:"enclosure"`
	Attributes []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"attr"`
}

func available(value string) bool {
	return strings.EqualFold(value, "yes") || strings.EqualFold(value, "true")
}

func parameterSet(value string) map[string]bool {
	result := make(map[string]bool)
	for _, parameter := range strings.Split(value, ",") {
		if parameter = strings.TrimSpace(strings.ToLower(parameter)); parameter != "" {
			result[parameter] = true
		}
	}
	return result
}

func parseOptionalInt(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

func parseOptionalFloat(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil
}

func parseInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func requestError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}

func torznabResponseError(body []byte) error {
	var response struct {
		XMLName     xml.Name
		Code        string `xml:"code,attr"`
		Description string `xml:"description,attr"`
	}
	if err := xml.Unmarshal(body, &response); err == nil && response.XMLName.Local == "error" {
		return fmt.Errorf("Torznab error %s: %s", response.Code, response.Description)
	}
	return nil
}
