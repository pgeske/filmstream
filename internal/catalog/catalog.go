package catalog

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type SearchRequest struct {
	Query       string      `json:"query"`
	Year        int         `json:"year,omitempty"`
	Preferences Preferences `json:"preferences"`
}

type Preferences struct {
	Resolution          string   `json:"resolution,omitempty"`
	Codecs              []string `json:"codecs,omitempty"`
	Languages           []string `json:"languages,omitempty"`
	MaxSizeBytes        int64    `json:"max_size_bytes,omitempty"`
	StreamingOptimized  bool     `json:"streaming_optimized,omitempty"`
	PreferTextSubtitles bool     `json:"prefer_text_subtitles,omitempty"`
}

type Candidate struct {
	ID                   string   `json:"id"`
	Indexer              string   `json:"indexer"`
	Name                 string   `json:"name"`
	Year                 int      `json:"year,omitempty"`
	SizeBytes            int64    `json:"size_bytes,omitempty"`
	Seeders              *int     `json:"seeders,omitempty"`
	Leechers             *int     `json:"leechers,omitempty"`
	Resolution           string   `json:"resolution,omitempty"`
	Codec                string   `json:"codec,omitempty"`
	Languages            []string `json:"languages,omitempty"`
	ReleaseGroup         string   `json:"release_group,omitempty"`
	DownloadVolumeFactor *float64 `json:"download_volume_factor,omitempty"`
	UploadVolumeFactor   *float64 `json:"upload_volume_factor,omitempty"`
	Trusted              bool     `json:"trusted,omitempty"`
	Popularity           int64    `json:"popularity,omitempty"`
	MagnetURI            string   `json:"magnet_uri,omitempty"`
	TorrentURL           string   `json:"torrent_url,omitempty"`
}

type RankedCandidate struct {
	Candidate Candidate `json:"candidate"`
	Score     float64   `json:"score"`
	Reasons   []string  `json:"reasons"`
}

var (
	resolutionPattern = regexp.MustCompile(`(?i)(?:^|[^0-9])(2160p|1080p|720p|480p)(?:[^0-9]|$)`)
	x265Pattern       = regexp.MustCompile(`(?i)(?:x265|h[ ._-]?265|hevc)`)
	x264Pattern       = regexp.MustCompile(`(?i)(?:x264|h[ ._-]?264|avc)`)
	releaseMarkers    = map[string]bool{
		"2160p": true, "1080p": true, "720p": true, "480p": true,
		"bluray": true, "brrip": true, "dvd": true, "dvdrip": true,
		"hdtv": true, "remux": true, "uhd": true, "web": true,
		"webdl": true, "webrip": true, "x264": true, "x265": true,
		"h264": true, "h265": true, "avc": true, "hevc": true,
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
	ranked := make([]RankedCandidate, 0, len(candidates))
	for _, raw := range candidates {
		candidate := Enrich(raw)
		if candidate.Year == 0 {
			candidate.Year = inferReleaseYear(request.Query, candidate.Name)
		}
		if request.Preferences.MaxSizeBytes > 0 && candidate.SizeBytes > request.Preferences.MaxSizeBytes {
			continue
		}
		if request.Preferences.StreamingOptimized {
			name := normalize(candidate.Name)
			words := wordSet(name)
			if hasWord(words, "dv") || hasWord(words, "dovi") || strings.Contains(name, "dolby vision") {
				continue
			}
			if len(request.Preferences.Codecs) > 0 {
				unknownUntrustedCodec := candidate.Codec == "" && !candidate.Trusted
				knownUnsupportedCodec := candidate.Codec != "" && !containsFold(request.Preferences.Codecs, candidate.Codec)
				if unknownUntrustedCodec || knownUnsupportedCodec {
					continue
				}
			}
		}
		if request.Year > 0 && candidate.Year > 0 && request.Year != candidate.Year {
			continue
		}

		match := titleSimilarity(request.Query, candidate.Name)
		if match < 0.25 {
			continue
		}
		score := match * 500
		reasons := []string{formatReason("title match", match*100)}

		if request.Year > 0 && candidate.Year == request.Year {
			score += 80
			reasons = append(reasons, "exact year")
		}

		if preferred := strings.ToLower(request.Preferences.Resolution); preferred != "" {
			switch strings.ToLower(candidate.Resolution) {
			case preferred:
				score += 50
				reasons = append(reasons, "preferred resolution")
			case "2160p":
				score += 15
			case "720p":
				score += 5
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
			words := wordSet(normalize(candidate.Name))
			if candidate.SizeBytes > 0 {
				sizeGiB := float64(candidate.SizeBytes) / float64(int64(1)<<30)
				penalty := math.Min(sizeGiB*0.5, 25)
				score -= penalty
				reasons = append(reasons, formatReason("streaming size tie-breaker", penalty))
			}
			if hasWord(words, "remux") {
				score -= 100
				reasons = append(reasons, "remux penalty")
			}
			if hasWord(words, "x264") || hasWord(words, "x265") {
				score += 20
				reasons = append(reasons, "streaming-friendly encode")
			}
		}

		if candidate.Seeders != nil {
			if *candidate.Seeders > 0 {
				seederWeight := 16.0
				if request.Preferences.StreamingOptimized {
					seederWeight = 24
				}
				score += math.Log2(float64(*candidate.Seeders)+1) * seederWeight
				reasons = append(reasons, formatReason("seeders", float64(*candidate.Seeders)))
			} else {
				score -= 60
				reasons = append(reasons, "no reported seeders")
			}
		}
		if candidate.Leechers != nil && *candidate.Leechers > 0 {
			// Active leechers improve the chance of uploading enough to meet a ratio target.
			score += math.Log2(float64(*candidate.Leechers)+1) * 5
			reasons = append(reasons, formatReason("leechers", float64(*candidate.Leechers)))
		}
		if !request.Preferences.StreamingOptimized && candidate.DownloadVolumeFactor != nil && *candidate.DownloadVolumeFactor < 1 {
			score += (1 - *candidate.DownloadVolumeFactor) * 35
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

	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})
	return ranked
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

	queryWords := wordSet(queryNormalized)
	candidateWords := wordSet(candidateTitle)
	intersection := 0
	for word := range queryWords {
		if _, ok := candidateWords[word]; ok {
			intersection++
		}
	}
	union := len(queryWords) + len(candidateWords) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func normalize(value string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(value) {
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
		if _, ok := parseReleaseYear(word); ok || releaseMarkers[word] {
			words = words[:index]
			break
		}
	}
	return strings.Join(words, " ")
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
