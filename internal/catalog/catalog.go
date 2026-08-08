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
	Resolution   string   `json:"resolution,omitempty"`
	Codecs       []string `json:"codecs,omitempty"`
	Languages    []string `json:"languages,omitempty"`
	MaxSizeBytes int64    `json:"max_size_bytes,omitempty"`
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
		if request.Preferences.MaxSizeBytes > 0 && candidate.SizeBytes > request.Preferences.MaxSizeBytes {
			continue
		}

		match := titleSimilarity(request.Query, candidate.Name)
		if match < 0.25 {
			continue
		}
		score := match * 500
		reasons := []string{formatReason("title match", match*100)}

		if request.Year > 0 && candidate.Year > 0 {
			if request.Year == candidate.Year {
				score += 80
				reasons = append(reasons, "exact year")
			} else {
				score -= math.Min(160, float64(abs(request.Year-candidate.Year))*40)
				reasons = append(reasons, "year mismatch")
			}
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

		if candidate.Seeders != nil {
			if *candidate.Seeders > 0 {
				score += math.Log2(float64(*candidate.Seeders)+1) * 16
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
		if candidate.DownloadVolumeFactor != nil && *candidate.DownloadVolumeFactor < 1 {
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
	candidateNormalized := normalize(candidate)
	if queryNormalized == "" || candidateNormalized == "" {
		return 0
	}
	if strings.Contains(candidateNormalized, queryNormalized) {
		return 1
	}

	queryWords := wordSet(queryNormalized)
	candidateWords := wordSet(candidateNormalized)
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

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
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

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
