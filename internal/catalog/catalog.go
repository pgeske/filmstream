package catalog

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type SearchRequest struct {
	Query            string      `json:"query"`
	Year             int         `json:"year,omitempty"`
	MediaType        string      `json:"media_type,omitempty"`
	SeasonNumber     int         `json:"season_number,omitempty"`
	EpisodeNumber    int         `json:"episode_number,omitempty"`
	PreferSeasonPack bool        `json:"prefer_season_pack,omitempty"`
	Preferences      Preferences `json:"preferences"`
	// IMDBID ("tt…") and TMDBID identify the movie or series so indexers that
	// support ID searches return exact matches.
	IMDBID string `json:"imdb_id,omitempty"`
	TMDBID int    `json:"tmdb_id,omitempty"`
	// RuntimeMinutes covers the requested movie or episode and
	// SeasonRuntimeMinutes the whole requested season. Ranking uses them to
	// estimate a release's bitrate; zero means unknown.
	RuntimeMinutes       int `json:"runtime_minutes,omitempty"`
	SeasonRuntimeMinutes int `json:"season_runtime_minutes,omitempty"`
}

type Preferences struct {
	Resolution          string   `json:"resolution,omitempty"`
	Codecs              []string `json:"codecs,omitempty"`
	Languages           []string `json:"languages,omitempty"`
	MaxSizeBytes        int64    `json:"max_size_bytes,omitempty"`
	StreamingOptimized  bool     `json:"streaming_optimized,omitempty"`
	PreferTextSubtitles bool     `json:"prefer_text_subtitles,omitempty"`
}

const (
	ProtocolTorrent        = "torrent"
	ProtocolUsenet         = "usenet"
	minimumTitleSimilarity = 0.7
)

type Candidate struct {
	ID                   string   `json:"id"`
	Indexer              string   `json:"indexer"`
	Name                 string   `json:"name"`
	Protocol             string   `json:"protocol,omitempty"`
	Year                 int      `json:"year,omitempty"`
	SizeBytes            int64    `json:"size_bytes,omitempty"`
	Seeders              *int     `json:"seeders,omitempty"`
	Leechers             *int     `json:"leechers,omitempty"`
	Resolution           string   `json:"resolution,omitempty"`
	Codec                string   `json:"codec,omitempty"`
	Languages            []string `json:"languages,omitempty"`
	Categories           []int    `json:"categories,omitempty"`
	ReleaseGroup         string   `json:"release_group,omitempty"`
	DownloadVolumeFactor *float64 `json:"download_volume_factor,omitempty"`
	UploadVolumeFactor   *float64 `json:"upload_volume_factor,omitempty"`
	Trusted              bool     `json:"trusted,omitempty"`
	Popularity           int64    `json:"popularity,omitempty"`
	PublishedUnix        int64    `json:"published_unix,omitempty"`
	// InfoHash is the lowercase hex BitTorrent v1 info hash when the indexer
	// or magnet link reveals it.
	InfoHash string `json:"info_hash,omitempty"`
	// Private marks a release from a private-tracker indexer.
	Private    bool   `json:"private,omitempty"`
	MagnetURI  string `json:"magnet_uri,omitempty"`
	TorrentURL string `json:"torrent_url,omitempty"`
	NZBURL     string `json:"nzb_url,omitempty"`
}

// Key identifies a release across indexers: its info hash when known,
// otherwise the indexer-scoped ID (or name).
func (c Candidate) Key() string {
	if c.InfoHash != "" {
		return "btih:" + strings.ToLower(c.InfoHash)
	}
	id := c.ID
	if id == "" {
		id = c.Name
	}
	return strings.ToLower(c.Indexer + ":" + id)
}

type RankedCandidate struct {
	Candidate Candidate `json:"candidate"`
	Score     float64   `json:"score"`
	Reasons   []string  `json:"reasons"`
}

type CandidateRejection struct {
	Candidate Candidate
	Reason    string
}

type RankingDiagnostics struct {
	Accepted         int
	Rejected         int
	RejectionReasons map[string]int
	Rejections       []CandidateRejection
}

const (
	rejectionMaxSize             = "max_size"
	rejectionNoSeeders           = "no_seeders"
	rejectionDolbyVision         = "dolby_vision"
	rejectionAIUpscale           = "ai_upscale"
	rejection2160pRemux          = "2160p_remux"
	rejectionUnknownCodec        = "unknown_codec"
	rejectionUnsupportedCodec    = "unsupported_codec"
	rejectionEpisodeMismatch     = "episode_mismatch"
	rejectionSeasonMismatch      = "season_mismatch"
	rejectionYearMismatch        = "year_mismatch"
	rejectionTitleMismatch       = "title_mismatch"
	rejectionSeasonPackPreferred = "season_pack_preferred"
)

const (
	// Torrents below lowSeeders are heavily penalized; zero seeders are rejected.
	lowSeeders = 3
	// healthySeeders is the smallest swarm treated as reliably streamable. A
	// season pack must reach it before it may displace individual episodes.
	healthySeeders = 5
	// Seeders beyond seederScoreCap no longer improve streaming reliability, so
	// they cannot outweigh the preferred resolution.
	seederScoreCap = 64
	// Estimated video bitrates above these limits are penalized: they exceed what
	// a torrent swarm reliably sustains through the VPN (1080p remuxes, 4K remuxes).
	maxStreamingMbps     = 15.0
	maxStreamingMbps2160 = 40.0
)

var (
	resolutionPattern    = regexp.MustCompile(`(?i)(?:^|[^0-9])(2160p|1080p|720p|480p)(?:[^0-9]|$)`)
	x265Pattern          = regexp.MustCompile(`(?i)(?:x265|h[ ._-]?265|hevc)`)
	x264Pattern          = regexp.MustCompile(`(?i)(?:x264|h[ ._-]?264|avc)`)
	seasonEpisodePattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})[ ._-]*e(\d{1,3})(?:[^0-9]|$)`)
	xEpisodePattern      = regexp.MustCompile(`(?i)(?:^|[^0-9])(\d{1,2})x(\d{1,3})(?:[^0-9]|$)`)
	seasonPattern        = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:s|season[ ._-]*)(\d{1,2})(?:[^0-9]|$)`)
	releaseMarkers       = map[string]bool{
		"2160p": true, "1080p": true, "720p": true, "480p": true,
		"bluray": true, "brrip": true, "dvd": true, "dvdrip": true,
		"hdtv": true, "remux": true, "uhd": true, "web": true,
		"webdl": true, "webrip": true, "x264": true, "x265": true,
		"h264": true, "h265": true, "avc": true, "hevc": true,
	}
	titleStopWords = map[string]bool{
		"a": true, "an": true, "and": true, "for": true, "in": true,
		"of": true, "on": true, "the": true, "to": true,
	}
)

func Enrich(candidate Candidate) Candidate {
	if candidate.Resolution == "" {
		if match := resolutionPattern.FindStringSubmatch(candidate.Name); len(match) > 1 {
			candidate.Resolution = strings.ToLower(match[1])
		}
	}
	if candidate.Codec == "" {
		switch {
		case x265Pattern.MatchString(candidate.Name):
			candidate.Codec = "h265"
		case x264Pattern.MatchString(candidate.Name):
			candidate.Codec = "h264"
		}
	}
	return candidate
}

func Rank(request SearchRequest, candidates []Candidate) []RankedCandidate {
	ranked, _ := RankWithDiagnostics(request, candidates)
	return ranked
}

func RankWithDiagnostics(request SearchRequest, candidates []Candidate) ([]RankedCandidate, RankingDiagnostics) {
	ranked := make([]RankedCandidate, 0, len(candidates))
	diagnostics := RankingDiagnostics{RejectionReasons: make(map[string]int)}
	reject := func(candidate Candidate, reason string) {
		diagnostics.Rejected++
		diagnostics.RejectionReasons[reason]++
		diagnostics.Rejections = append(diagnostics.Rejections, CandidateRejection{
			Candidate: candidate,
			Reason:    reason,
		})
	}
	now := time.Now()
	for _, raw := range candidates {
		candidate := Enrich(raw)
		if candidate.Year == 0 {
			candidate.Year = inferReleaseYear(request.Query, candidate.Name)
		}
		if request.Preferences.MaxSizeBytes > 0 && candidate.SizeBytes > request.Preferences.MaxSizeBytes {
			reject(candidate, rejectionMaxSize)
			continue
		}
		// A swarm without seeders cannot stream; it would only burn the startup budget.
		if candidate.Protocol != ProtocolUsenet && candidate.Seeders != nil && *candidate.Seeders <= 0 {
			reject(candidate, rejectionNoSeeders)
			continue
		}
		if request.Preferences.StreamingOptimized {
			name := normalize(candidate.Name)
			words := wordSet(name)
			if hasWord(words, "dv") || hasWord(words, "dovi") || strings.Contains(name, "dolby vision") {
				reject(candidate, rejectionDolbyVision)
				continue
			}
			if isUpscaledRelease(name) {
				reject(candidate, rejectionAIUpscale)
				continue
			}
			if strings.EqualFold(candidate.Resolution, "2160p") && hasWord(words, "remux") {
				reject(candidate, rejection2160pRemux)
				continue
			}
			if len(request.Preferences.Codecs) > 0 {
				unknownUntrustedCodec := candidate.Codec == "" && !candidate.Trusted
				knownUnsupportedCodec := candidate.Codec != "" && !containsFold(request.Preferences.Codecs, candidate.Codec)
				switch {
				case unknownUntrustedCodec:
					reject(candidate, rejectionUnknownCodec)
					continue
				case knownUnsupportedCodec:
					reject(candidate, rejectionUnsupportedCodec)
					continue
				}
			}
		}
		episodeMatch := false
		seasonPack := false
		if request.SeasonNumber > 0 && request.EpisodeNumber > 0 {
			season, episode, hasEpisode := releaseEpisode(candidate.Name)
			switch {
			case hasEpisode && season == request.SeasonNumber && episode == request.EpisodeNumber:
				episodeMatch = true
			case hasEpisode:
				reject(candidate, rejectionEpisodeMismatch)
				continue
			default:
				candidateSeason, hasSeason := releaseSeason(candidate.Name)
				if !hasSeason || candidateSeason != request.SeasonNumber {
					reject(candidate, rejectionSeasonMismatch)
					continue
				}
				seasonPack = true
			}
		}
		if request.MediaType != "show" && request.Year > 0 && candidate.Year > 0 && request.Year != candidate.Year {
			reject(candidate, rejectionYearMismatch)
			continue
		}

		match := titleSimilarity(request.Query, candidate.Name)
		if match < minimumTitleSimilarity {
			reject(candidate, rejectionTitleMismatch)
			continue
		}
		score := match * 500
		reasons := []string{formatReason("title match", match*100)}
		if episodeMatch {
			score += 250
			reasons = append(reasons, "exact episode")
		} else if seasonPack {
			score -= 80
			reasons = append(reasons, "season pack")
		}

		if request.MediaType != "show" && request.Year > 0 && candidate.Year == request.Year {
			score += 80
			reasons = append(reasons, "exact year")
		}

		if preferred := strings.ToLower(request.Preferences.Resolution); preferred != "" {
			switch strings.ToLower(candidate.Resolution) {
			case preferred:
				score += 300
				reasons = append(reasons, "preferred resolution")
			case "2160p":
				// Streaming 4K instead of a preferred lower resolution costs
				// roughly three times the bandwidth.
				if request.Preferences.StreamingOptimized {
					score += 80
				} else {
					score += 160
				}
				reasons = append(reasons, "alternate 2160p quality")
			case "720p":
				score += 120
				reasons = append(reasons, "alternate 720p quality")
			case "480p":
				score += 40
			}
		}

		if containsFold(request.Preferences.Codecs, candidate.Codec) {
			score += 12
			reasons = append(reasons, "supported codec")
		}
		if languageOverlap(request.Preferences.Languages, candidate.Languages) {
			score += 20
			reasons = append(reasons, "preferred language")
		}
		if request.Preferences.StreamingOptimized {
			if candidate.Protocol == ProtocolUsenet {
				score += 500
				reasons = append(reasons, "preferred Usenet source")
				if candidate.PublishedUnix > 0 {
					age := now.Sub(time.Unix(candidate.PublishedUnix, 0))
					switch {
					case age <= 2*365*24*time.Hour:
						score += 90
						reasons = append(reasons, "recent Usenet post")
					case age <= 5*365*24*time.Hour:
						score += 60
						reasons = append(reasons, "recent Usenet post")
					case age <= 10*365*24*time.Hour:
						score += 20
					default:
						score -= 80
						reasons = append(reasons, "old Usenet post")
					}
				}
			}
			words := wordSet(normalize(candidate.Name))
			remux := hasWord(words, "remux")
			if mbps, known := estimatedMbps(request, candidate, seasonPack); known {
				limit := maxStreamingMbps
				if strings.EqualFold(candidate.Resolution, "2160p") {
					limit = maxStreamingMbps2160
				}
				if mbps > limit {
					score -= min(300, (mbps/limit-1)*250)
					reasons = append(reasons, formatReason("high bitrate Mbps", math.Round(mbps)))
				}
			} else if remux {
				// Without a runtime, the remux label is the only bitrate signal.
				score -= 250
				reasons = append(reasons, "remux penalty")
			}
			if !remux && streamingFriendlyEncode(words, candidate.Codec) {
				score += 40
				reasons = append(reasons, "streaming-friendly encode")
			}
		}

		if candidate.Seeders != nil && *candidate.Seeders > 0 {
			seederWeight := 30.0
			if request.Preferences.StreamingOptimized {
				seederWeight = 60
			}
			score += math.Log2(float64(min(*candidate.Seeders, seederScoreCap))+1) * seederWeight
			reasons = append(reasons, formatReason("seeders", float64(*candidate.Seeders)))
			if *candidate.Seeders < lowSeeders {
				score -= 150
				reasons = append(reasons, "few seeders")
			}
		}
		if candidate.Leechers != nil && *candidate.Leechers > 0 {
			// Active leechers improve the chance of uploading enough to meet a ratio target.
			score += math.Log2(float64(*candidate.Leechers)+1) * 5
			reasons = append(reasons, formatReason("leechers", float64(*candidate.Leechers)))
		}
		if candidate.Private {
			// Private trackers are seeded by dedicated seedboxes and enforce
			// ratios, so their swarms are faster than public ones of similar size.
			score += 100
			reasons = append(reasons, "private tracker")
		}
		if candidate.DownloadVolumeFactor != nil && *candidate.DownloadVolumeFactor < 1 {
			score += (1 - max(0, *candidate.DownloadVolumeFactor)) * 40
			reasons = append(reasons, formatReason("download factor", *candidate.DownloadVolumeFactor))
		}
		if candidate.UploadVolumeFactor != nil && *candidate.UploadVolumeFactor > 1 {
			score += math.Log2(*candidate.UploadVolumeFactor) * 10
			reasons = append(reasons, formatReason("upload factor", *candidate.UploadVolumeFactor))
		}
		if candidate.Trusted {
			score += 75
			reasons = append(reasons, "trusted catalog")
		}
		if candidate.Popularity > 0 {
			score += math.Log2(float64(candidate.Popularity)+1) * 2
		}

		ranked = append(ranked, RankedCandidate{Candidate: candidate, Score: score, Reasons: reasons})
	}

	if request.PreferSeasonPack && request.SeasonNumber > 0 {
		// Prefer a season pack only when a healthy one exists; a dead pack must not
		// hide healthy individual episode releases.
		healthyPack := false
		for _, candidate := range ranked {
			if IsSeasonPack(candidate.Candidate.Name, request.SeasonNumber) && HealthySwarm(candidate.Candidate) {
				healthyPack = true
				break
			}
		}
		if healthyPack {
			seasonPacks := make([]RankedCandidate, 0, len(ranked))
			for _, candidate := range ranked {
				if IsSeasonPack(candidate.Candidate.Name, request.SeasonNumber) {
					seasonPacks = append(seasonPacks, candidate)
				} else {
					reject(candidate.Candidate, rejectionSeasonPackPreferred)
				}
			}
			ranked = seasonPacks
		}
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})
	diagnostics.Accepted = len(ranked)
	return ranked, diagnostics
}

func IsSeasonPack(name string, seasonNumber int) bool {
	if _, _, hasEpisode := releaseEpisode(name); hasEpisode {
		return false
	}
	season, hasSeason := releaseSeason(name)
	return hasSeason && season == seasonNumber
}

// HealthySwarm reports whether a release's reported swarm is large enough to
// stream reliably. Unknown seeder counts (Usenet, curated catalogs) count as healthy.
func HealthySwarm(candidate Candidate) bool {
	return candidate.Seeders == nil || *candidate.Seeders >= healthySeeders
}

// estimatedMbps derives a release's average bitrate from its size and the
// runtime it covers: the whole season for a season pack.
func estimatedMbps(request SearchRequest, candidate Candidate, seasonPack bool) (float64, bool) {
	runtime := request.RuntimeMinutes
	if seasonPack {
		runtime = request.SeasonRuntimeMinutes
	}
	if runtime <= 0 || candidate.SizeBytes <= 0 {
		return 0, false
	}
	return float64(candidate.SizeBytes) * 8 / (float64(runtime) * 60) / 1e6, true
}

// streamingFriendlyEncode reports an H.264/HEVC re-encode of a web or Blu-ray
// source: compact, widely seeded, and natively decodable by Apple TV.
func streamingFriendlyEncode(words map[string]struct{}, codec string) bool {
	if codec != "h264" && codec != "h265" {
		return false
	}
	for _, source := range []string{"web", "webdl", "webrip", "bluray", "bdrip", "brrip"} {
		if hasWord(words, source) {
			return true
		}
	}
	return hasWord(words, "blu") && hasWord(words, "ray")
}

func isUpscaledRelease(normalizedName string) bool {
	words := wordSet(normalizedName)
	if hasWord(words, "upscale") || hasWord(words, "upscaled") || hasWord(words, "upscaling") {
		return true
	}
	compact := strings.ReplaceAll(normalizedName, " ", "")
	return strings.Contains(compact, "aiupscal")
}

func titleSimilarity(query, candidate string) float64 {
	queryNormalized := normalize(query)
	candidateTitle := releaseTitle(queryNormalized, normalize(candidate))
	if queryNormalized == "" || candidateTitle == "" {
		return 0
	}
	if candidateTitle == queryNormalized {
		return 1
	}

	queryWords := meaningfulTitleWords(queryNormalized)
	candidateWords := meaningfulTitleWords(candidateTitle)
	intersection := 0
	for word := range queryWords {
		if _, ok := candidateWords[word]; ok {
			intersection++
		}
	}
	wordCount := len(queryWords) + len(candidateWords)
	if wordCount == 0 {
		return 0
	}
	return float64(2*intersection) / float64(wordCount)
}

func normalize(value string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(value) {
		if r == '\'' || r == '’' {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func wordSet(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, word := range strings.Fields(value) {
		result[word] = struct{}{}
	}
	return result
}

func meaningfulTitleWords(value string) map[string]struct{} {
	all := wordSet(value)
	meaningful := make(map[string]struct{}, len(all))
	for word := range all {
		if !titleStopWords[word] {
			meaningful[word] = struct{}{}
		}
	}
	if len(meaningful) == 0 {
		return all
	}
	return meaningful
}

func inferReleaseYear(query, candidate string) int {
	queryWords := wordSet(normalize(query))
	year := 0
	for _, word := range strings.Fields(normalize(candidate)) {
		if _, partOfTitle := queryWords[word]; partOfTitle {
			continue
		}
		if releaseMarkers[word] {
			break
		}
		if parsed, ok := parseReleaseYear(word); ok {
			year = parsed
		}
	}
	return year
}

func releaseTitle(normalizedQuery, normalizedCandidate string) string {
	queryWords := wordSet(normalizedQuery)
	words := strings.Fields(normalizedCandidate)
	for index, word := range words {
		if _, partOfTitle := queryWords[word]; partOfTitle {
			continue
		}
		if _, _, ok := releaseEpisode(word); ok || isSeasonToken(word) {
			words = words[:index]
			break
		}
		if _, ok := parseReleaseYear(word); ok || releaseMarkers[word] {
			words = words[:index]
			break
		}
	}
	return strings.Join(words, " ")
}

func releaseEpisode(value string) (int, int, bool) {
	for _, pattern := range []*regexp.Regexp{seasonEpisodePattern, xEpisodePattern} {
		match := pattern.FindStringSubmatch(value)
		if len(match) != 3 {
			continue
		}
		season, seasonErr := strconv.Atoi(match[1])
		episode, episodeErr := strconv.Atoi(match[2])
		if seasonErr == nil && episodeErr == nil && season > 0 && episode > 0 {
			return season, episode, true
		}
	}
	return 0, 0, false
}

func releaseSeason(value string) (int, bool) {
	match := seasonPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0, false
	}
	season, err := strconv.Atoi(match[1])
	return season, err == nil && season > 0
}

func isSeasonToken(value string) bool {
	if _, _, ok := releaseEpisode(value); ok {
		return true
	}
	_, ok := releaseSeason(value)
	return ok
}

func parseReleaseYear(value string) (int, bool) {
	if len(value) != 4 || value[0] != '1' && value[0] != '2' {
		return 0, false
	}
	year, err := strconv.Atoi(value)
	if err != nil || year < 1900 || year > 2099 {
		return 0, false
	}
	return year, true
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func hasWord(words map[string]struct{}, word string) bool {
	_, ok := words[word]
	return ok
}

func languageOverlap(preferred, actual []string) bool {
	if len(actual) == 0 {
		return false
	}
	for _, want := range preferred {
		for _, got := range actual {
			if strings.EqualFold(want, got) || strings.HasPrefix(strings.ToLower(got), strings.ToLower(want)+"-") {
				return true
			}
		}
	}
	return false
}

func formatReason(label string, value float64) string {
	precision := 1
	if value == math.Trunc(value) {
		precision = 0
	}
	return label + ": " + strconv.FormatFloat(value, 'f', precision, 64)
}
