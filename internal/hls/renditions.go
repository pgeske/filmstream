package hls

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Native HLS subtitle renditions. The packager writes every text track as one
// growing WebVTT file in lockstep with the video. That file stays available
// whole for clients that parse it themselves, and is exposed here as segmented
// WebVTT media playlists aligned with the video segments plus a master playlist.

const (
	playlistContentType = "application/vnd.apple.mpegurl"
	webVTTContentType   = "text/vtt; charset=utf-8"
	subtitleGroupID     = "subs"
)

type assetKind int

const (
	assetMediaPlaylist assetKind = iota
	assetMasterPlaylist
	assetInitSegment
	assetMediaSegment
	assetSubtitleFile
	assetSubtitlePlaylist
	assetSubtitleSegment
)

type assetName struct {
	kind          assetKind
	subtitleIndex int
	sequence      int
}

func (name assetName) contentType() string {
	switch name.kind {
	case assetMediaSegment:
		return "video/iso.segment"
	case assetInitSegment:
		return "video/mp4"
	case assetSubtitleFile:
		return webVTTContentType
	default:
		return ""
	}
}

var (
	mediaSegmentPattern     = regexp.MustCompile(`^segment-([0-9]{6,9})\.m4s$`)
	subtitleFilePattern     = regexp.MustCompile(`^subtitle-([0-9]{1,4})\.vtt$`)
	subtitlePlaylistPattern = regexp.MustCompile(`^subs-([0-9]{1,4})\.m3u8$`)
	subtitleSegmentPattern  = regexp.MustCompile(`^subs-([0-9]{1,4})-([0-9]{1,6})\.vtt$`)
)

// parseAssetName accepts only names the packager or the playlists produce.
func parseAssetName(name string) (assetName, bool) {
	switch name {
	case "index.m3u8":
		return assetName{kind: assetMediaPlaylist, subtitleIndex: -1}, true
	case "master.m3u8":
		return assetName{kind: assetMasterPlaylist, subtitleIndex: -1}, true
	case "init.mp4":
		return assetName{kind: assetInitSegment, subtitleIndex: -1}, true
	}
	if match := mediaSegmentPattern.FindStringSubmatch(name); match != nil {
		sequence, _ := strconv.Atoi(match[1])
		return assetName{kind: assetMediaSegment, subtitleIndex: -1, sequence: sequence}, true
	}
	if match := subtitleFilePattern.FindStringSubmatch(name); match != nil {
		index, _ := strconv.Atoi(match[1])
		return assetName{kind: assetSubtitleFile, subtitleIndex: index}, true
	}
	if match := subtitlePlaylistPattern.FindStringSubmatch(name); match != nil {
		index, _ := strconv.Atoi(match[1])
		return assetName{kind: assetSubtitlePlaylist, subtitleIndex: index}, true
	}
	if match := subtitleSegmentPattern.FindStringSubmatch(name); match != nil {
		index, _ := strconv.Atoi(match[1])
		sequence, _ := strconv.Atoi(match[2])
		return assetName{kind: assetSubtitleSegment, subtitleIndex: index, sequence: sequence}, true
	}
	return assetName{}, false
}

func subtitleFileName(index int) string {
	return fmt.Sprintf("subtitle-%d.vtt", index)
}

// withPlaybackStart makes players begin an event playlist at its first
// segment instead of near the live edge.
func withPlaybackStart(playlist []byte) []byte {
	contents := string(playlist)
	if strings.Contains(contents, "#EXT-X-START:") {
		return playlist
	}
	return []byte(strings.Replace(contents, "#EXTM3U\n", "#EXTM3U\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n", 1))
}

// masterPlaylist lists the packaged media playlist as the only variant and
// each text subtitle track as a WebVTT rendition in one group.
func masterPlaylist(subtitles []SubtitleTrack, bandwidth int) []byte {
	var builder strings.Builder
	builder.WriteString("#EXTM3U\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	renditions := 0
	for _, track := range subtitles {
		if track.Kind != "text" || track.RenditionName == "" {
			continue
		}
		renditions++
		fmt.Fprintf(&builder, `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="%s",NAME="%s"`, subtitleGroupID, track.RenditionName)
		if language := renditionLanguage(track.Language); language != "" {
			fmt.Fprintf(&builder, `,LANGUAGE="%s"`, language)
		}
		fmt.Fprintf(&builder, `,DEFAULT=NO,AUTOSELECT=YES,FORCED=%s,URI="subs-%d.m3u8"`+"\n",
			yesNo(track.Forced), track.Index)
	}
	fmt.Fprintf(&builder, "#EXT-X-STREAM-INF:BANDWIDTH=%d", bandwidth)
	if renditions > 0 {
		fmt.Fprintf(&builder, `,SUBTITLES="%s"`, subtitleGroupID)
	}
	builder.WriteString("\nindex.m3u8\n")
	return []byte(builder.String())
}

// publishedSubtitleSegments trails the video by one segment until the
// packager exits: a cue is written only once the demuxer reaches it, and
// sources may interleave subtitle packets slightly after the video they
// accompany. A published subtitle segment is therefore already complete.
func publishedSubtitleSegments(video mediaPlaylist, final bool) int {
	if final {
		return len(video.durations)
	}
	return max(0, len(video.durations)-1)
}

func subtitlePlaylist(video mediaPlaylist, index int, final bool) []byte {
	count := publishedSubtitleSegments(video, final)
	targetDuration := video.targetDuration
	for _, duration := range video.durations[:count] {
		targetDuration = max(targetDuration, int(math.Round(duration)))
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n", max(1, targetDuration))
	builder.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for sequence, duration := range video.durations[:count] {
		fmt.Fprintf(&builder, "#EXTINF:%s,\nsubs-%d-%d.vtt\n",
			strconv.FormatFloat(duration, 'f', 6, 64), index, sequence)
	}
	if final {
		builder.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(builder.String())
}

// subtitleSegmentWindow returns a published subtitle segment's span in player
// time, which starts at zero with the first video segment. The first and the
// final segment are open-ended so cues around the edges are never lost.
func subtitleSegmentWindow(video mediaPlaylist, sequence int, final bool) (float64, float64, bool) {
	count := publishedSubtitleSegments(video, final)
	if sequence < 0 || sequence >= count {
		return 0, 0, false
	}
	start := 0.0
	for _, duration := range video.durations[:sequence] {
		start += duration
	}
	end := start + video.durations[sequence]
	if sequence == 0 {
		start = math.Inf(-1)
	}
	if final && sequence == count-1 {
		end = math.Inf(1)
	}
	return start, end, true
}

type webVTTCue struct {
	start    float64
	end      float64
	settings string
	payload  string
}

// parseWebVTTCues reads the cues of a growing WebVTT file. A block counts only
// once it is terminated by a blank line, so a cue FFmpeg is still writing is skipped.
func parseWebVTTCues(contents []byte) []webVTTCue {
	text := strings.ReplaceAll(string(contents), "\r\n", "\n")
	complete := strings.LastIndex(text, "\n\n")
	if complete < 0 {
		return nil
	}
	var cues []webVTTCue
	for _, block := range strings.Split(text[:complete], "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		for number, line := range lines {
			if !strings.Contains(line, "-->") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[1] != "-->" {
				break
			}
			start, startOK := parseWebVTTTimestamp(fields[0])
			end, endOK := parseWebVTTTimestamp(fields[2])
			payload := strings.Join(lines[number+1:], "\n")
			if startOK && endOK && end >= start && strings.TrimSpace(payload) != "" {
				cues = append(cues, webVTTCue{
					start: start, end: end, settings: strings.Join(fields[3:], " "), payload: payload,
				})
			}
			break
		}
	}
	return cues
}

func parseWebVTTTimestamp(value string) (float64, bool) {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || seconds < 0 || seconds >= 60 {
		return 0, false
	}
	multiplier := 60.0
	for position := len(parts) - 2; position >= 0; position-- {
		value, err := strconv.Atoi(parts[position])
		if err != nil || value < 0 {
			return 0, false
		}
		seconds += float64(value) * multiplier
		multiplier *= 60
	}
	return seconds, true
}

func formatWebVTTTimestamp(seconds float64) string {
	milliseconds := int64(math.Round(max(0, seconds) * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d",
		milliseconds/3_600_000, milliseconds/60_000%60, milliseconds/1000%60, milliseconds%1000)
}

// subtitleSegment writes the cues overlapping [windowStart, windowEnd) of
// player time. Cue times are rewritten from media time to player time (zero at
// the first packaged video frame), and X-TIMESTAMP-MAP ties player time zero to
// that frame's packaged presentation time on the 90 kHz clock. A cue spanning a
// boundary is repeated in each segment it overlaps.
func subtitleSegment(
	cues []webVTTCue,
	windowStart float64,
	windowEnd float64,
	mediaOriginSeconds float64,
	packagedOriginSeconds float64,
) []byte {
	var builder strings.Builder
	fmt.Fprintf(&builder, "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000\n\n",
		int64(math.Round(max(0, packagedOriginSeconds)*90_000)))
	for _, cue := range cues {
		start := cue.start - mediaOriginSeconds
		end := cue.end - mediaOriginSeconds
		if end <= 0 || end <= windowStart || start >= windowEnd {
			continue
		}
		builder.WriteString(formatWebVTTTimestamp(start))
		builder.WriteString(" --> ")
		builder.WriteString(formatWebVTTTimestamp(end))
		if cue.settings != "" {
			builder.WriteString(" " + cue.settings)
		}
		builder.WriteString("\n" + cue.payload + "\n\n")
	}
	return []byte(builder.String())
}

// assignRenditionNames gives every text track a NAME that is unique within the
// subtitle group, as HLS requires; clients match renditions to tracks by it.
func assignRenditionNames(tracks []SubtitleTrack) {
	used := make(map[string]bool)
	for index := range tracks {
		if tracks[index].Kind != "text" {
			continue
		}
		base := renditionBaseName(tracks[index])
		name := base
		for number := 2; used[name]; number++ {
			name = fmt.Sprintf("%s %d", base, number)
		}
		used[name] = true
		tracks[index].RenditionName = name
	}
}

func renditionBaseName(track SubtitleTrack) string {
	title := strings.Join(strings.Fields(strings.ReplaceAll(track.Title, `"`, "'")), " ")
	language := languageDisplayName(track.Language)
	name := title
	switch {
	case title == "" && language != "":
		name = language
	case title == "":
		name = "Subtitles"
	case language != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(language)):
		name = language + " (" + title + ")"
	}
	if track.Forced && !strings.Contains(strings.ToLower(name), "forced") {
		name += " (Forced)"
	}
	return name
}

var languageTag = regexp.MustCompile(`^[a-z]{2,3}(-[a-zA-Z0-9]{1,8})*$`)

func renditionLanguage(language string) string {
	if language == "und" || !languageTag.MatchString(language) {
		return ""
	}
	return language
}

func languageDisplayName(language string) string {
	names := map[string]string{
		"ar": "Arabic", "bg": "Bulgarian", "ca": "Catalan", "cs": "Czech", "da": "Danish",
		"de": "German", "el": "Greek", "en": "English", "es": "Spanish", "et": "Estonian",
		"fa": "Persian", "fi": "Finnish", "fr": "French", "he": "Hebrew", "hi": "Hindi",
		"hr": "Croatian", "hu": "Hungarian", "id": "Indonesian", "is": "Icelandic", "it": "Italian",
		"ja": "Japanese", "ko": "Korean", "lt": "Lithuanian", "lv": "Latvian", "mk": "Macedonian",
		"ms": "Malay", "nl": "Dutch", "no": "Norwegian", "pl": "Polish", "pt": "Portuguese",
		"ro": "Romanian", "ru": "Russian", "sk": "Slovak", "sl": "Slovenian", "sr": "Serbian",
		"sv": "Swedish", "th": "Thai", "tr": "Turkish", "uk": "Ukrainian", "vi": "Vietnamese",
		"zh": "Chinese",
	}
	if name := names[language]; name != "" {
		return name
	}
	if language == "" || language == "und" || !languageTag.MatchString(language) {
		return ""
	}
	return strings.ToUpper(language)
}

func yesNo(value bool) string {
	if value {
		return "YES"
	}
	return "NO"
}
