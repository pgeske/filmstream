package torrentstream

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

var videoExtensions = map[string]bool{
	".avi": true, ".m4v": true, ".mkv": true, ".mov": true,
	".mp4": true, ".mpeg": true, ".mpg": true, ".ts": true, ".webm": true,
}

// selectVideoFile picks the playback file: among supported videos matching the
// hint (an episode code or name fragment), the largest one.
func selectVideoFile(files []pluginFile, fileHint string) (pluginFile, error) {
	var videos []pluginFile
	for _, file := range files {
		if !file.Pad && file.Size > 0 && videoExtensions[strings.ToLower(path.Ext(file.Path))] {
			videos = append(videos, file)
		}
	}
	if len(videos) == 0 {
		return pluginFile{}, errors.New("torrent contains no supported video files")
	}
	if strings.TrimSpace(fileHint) != "" && len(videos) > 1 {
		matching := matchingVideoFiles(videos, fileHint)
		if len(matching) == 0 {
			return pluginFile{}, fmt.Errorf("torrent contains no video matching %s", fileHint)
		}
		videos = matching
	}
	sort.SliceStable(videos, func(i, j int) bool {
		return videos[i].Size > videos[j].Size
	})
	return videos[0], nil
}

func matchingVideoFiles(files []pluginFile, hint string) []pluginFile {
	hint = normalizeFileHint(hint)
	if hint == "" {
		return nil
	}
	var matching []pluginFile
	for _, file := range files {
		name := normalizeFileHint(file.Path)
		if strings.Contains(name, hint) || matchesAlternateEpisodeCode(name, hint) {
			matching = append(matching, file)
		}
	}
	return matching
}

func normalizeFileHint(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func matchesAlternateEpisodeCode(name, hint string) bool {
	if len(hint) < 6 || hint[0] != 's' {
		return false
	}
	episodeIndex := strings.IndexByte(hint, 'e')
	if episodeIndex < 2 || episodeIndex == len(hint)-1 {
		return false
	}
	seasonRaw := hint[1:episodeIndex]
	episodeRaw := hint[episodeIndex+1:]
	season := strings.TrimLeft(seasonRaw, "0")
	episode := strings.TrimLeft(episodeRaw, "0")
	if season == "" {
		season = "0"
	}
	if episode == "" {
		episode = "0"
	}
	return strings.Contains(name, season+"x"+episode) ||
		strings.Contains(name, season+"x"+episodeRaw) ||
		strings.Contains(name, seasonRaw+"x"+episode) ||
		strings.Contains(name, seasonRaw+"x"+episodeRaw)
}

// displayPath is a file's path inside the torrent without the torrent's own
// top-level directory, like the file name a player shows.
func displayPath(torrentName string, files []pluginFile, file pluginFile) string {
	if len(files) > 1 {
		if trimmed, ok := strings.CutPrefix(file.Path, torrentName+"/"); ok && trimmed != "" {
			return trimmed
		}
	}
	return file.Path
}
