package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/history"
	"github.com/pgeske/filmstream/internal/hls"
	"github.com/pgeske/filmstream/internal/metadata"
)

const (
	prewarmRequestHeader = "X-Filmstream-Prewarm"
	prewarmBufferSeconds = 30
	prewarmMaxAge        = 20 * time.Minute
	prewarmHintTTL       = 30 * time.Minute
)

type playbackPrewarmTarget struct {
	request           CreatePlaybackRequest
	source            string
	priority          bool
	seed              *CreatePlaybackResponse
	subtitleSelection playbackSubtitleSelection
}

type playbackPrewarmState struct {
	target              playbackPrewarmTarget
	playbackReady       chan struct{}
	response            CreatePlaybackResponse
	err                 error
	claimed             bool
	bufferReady         bool
	bufferedAt          time.Time
	bitmapSubtitleIndex int
	cancel              context.CancelFunc
	parkCancel          context.CancelFunc
	parkDone            chan struct{}
}

func (s *Server) StartPlaybackPrewarmer(ctx context.Context, baseURL string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || s.historyStore == nil {
		return
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 8
	s.prewarmMu.Lock()
	s.prewarmBaseURL = baseURL
	s.prewarmClient = &http.Client{Transport: transport}
	s.prewarmContext = ctx
	s.prewarmMu.Unlock()

	go func() {
		<-ctx.Done()
		s.cancelPlaybackPrewarming()
	}()
}

func (s *Server) prewarmPlayback(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request CreatePlaybackRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	request.MediaID = strings.TrimSpace(request.MediaID)
	request.Query = strings.TrimSpace(request.Query)
	request.SeriesID = strings.TrimSpace(request.SeriesID)
	request.SeriesTitle = strings.TrimSpace(request.SeriesTitle)
	if request.MediaID == "" || request.Query == "" || request.StartSeconds < 0 {
		writeError(w, http.StatusBadRequest, "media_id, query, and a non-negative start_seconds are required")
		return
	}
	request.Preferences = mergePreferences(s.defaults, request.Preferences)
	if request.MediaType == string(metadata.MediaTypeShow) {
		// Browsing a show warms only the indexer search. Torrent media is mounted and
		// subtitle-probed after an explicit play request; active playback separately
		// buffers only its next episode.
		s.queueShowReleaseSearch(request)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "finding_releases"})
		return
	}
	s.queuePlaybackPrewarm(playbackPrewarmTarget{
		request: request, source: "hint", priority: true,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "prewarming"})
}

func (s *Server) claimPrewarmedPlayback(ctx context.Context, request CreatePlaybackRequest) (CreatePlaybackResponse, bool) {
	key := playbackPrewarmKey(request)
	if key == "" {
		return CreatePlaybackResponse{}, false
	}
	s.prewarmMu.Lock()
	state := s.prewarmStates[key]
	if state == nil || !matchingPrewarmRequest(state.target.request, request) {
		s.prewarmMu.Unlock()
		return CreatePlaybackResponse{}, false
	}
	ready := state.playbackReady
	s.prewarmMu.Unlock()

	select {
	case <-ctx.Done():
		return CreatePlaybackResponse{}, false
	case <-ready:
	}

	s.prewarmMu.Lock()
	state = s.prewarmStates[key]
	if state == nil || state.err != nil || state.response.ID == "" ||
		!matchingPrewarmRequest(state.target.request, request) {
		s.prewarmMu.Unlock()
		return CreatePlaybackResponse{}, false
	}
	state.claimed = true
	parkCancel := state.parkCancel
	parkDone := state.parkDone
	delete(s.prewarmStates, key)
	response := state.response
	s.prewarmMu.Unlock()

	if parkCancel != nil {
		parkCancel()
	}
	if parkDone != nil {
		select {
		case <-parkDone:
		case <-ctx.Done():
			state.cancel()
			go func() {
				<-parkDone
				s.stopPreparedPlayback(response.ID)
			}()
			return CreatePlaybackResponse{}, false
		}
	}
	if !s.playbackExists(response.ID) {
		return CreatePlaybackResponse{}, false
	}
	s.logger.Info("claimed prewarmed playback", "id", response.ID,
		"media_id", request.MediaID, "start_seconds", request.StartSeconds)
	return response, true
}

func (s *Server) queuePlaybackPrewarm(target playbackPrewarmTarget) {
	target.request.MediaID = strings.TrimSpace(target.request.MediaID)
	target.request.Query = strings.TrimSpace(target.request.Query)
	target.request.Preferences = mergePreferences(s.defaults, target.request.Preferences)
	key := playbackPrewarmKey(target.request)
	if key == "" || target.request.Query == "" || target.request.StartSeconds < 0 {
		return
	}

	s.prewarmMu.Lock()
	if s.prewarmContext == nil || s.prewarmClient == nil || s.prewarmBaseURL == "" {
		s.prewarmMu.Unlock()
		return
	}
	if existing := s.prewarmStates[key]; existing != nil && matchingQueuedPrewarmTarget(existing.target, target) {
		if existing.response.ID == "" || !existing.bufferReady {
			s.prewarmMu.Unlock()
			return
		}
		if time.Since(existing.bufferedAt) < prewarmMaxAge && s.prewarmedPlaybackAvailable(existing) {
			s.prewarmMu.Unlock()
			return
		}
	}
	stalePlaybackID := ""
	if existing := s.prewarmStates[key]; existing != nil {
		stalePlaybackID = existing.response.ID
		// A subtitle-mode change only needs new HLS packaging, not another source session.
		if target.seed == nil && existing.bufferReady && stalePlaybackID != "" &&
			matchingPrewarmRequest(existing.target.request, target.request) {
			seed := existing.response
			target.seed = &seed
		}
		delete(s.prewarmStates, key)
		existing.cancel()
	}
	stateContext, cancel := context.WithCancel(s.prewarmContext)
	state := &playbackPrewarmState{
		target: target, playbackReady: make(chan struct{}), cancel: cancel,
		bitmapSubtitleIndex: -1,
	}
	s.prewarmStates[key] = state
	s.prewarmMu.Unlock()

	if stalePlaybackID != "" {
		s.stopPreparedPlayback(stalePlaybackID)
	}
	go s.runPlaybackPrewarm(stateContext, key, state)
}

func (s *Server) runPlaybackPrewarm(ctx context.Context, key string, state *playbackPrewarmState) {
	operationContext, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	ctx = operationContext
	if !state.target.priority {
		select {
		case s.prewarmSlots <- struct{}{}:
			defer func() { <-s.prewarmSlots }()
		case <-ctx.Done():
			s.finishPlaybackPrewarm(key, state, CreatePlaybackResponse{}, ctx.Err())
			return
		}
	}

	response := CreatePlaybackResponse{}
	if state.target.seed != nil && s.playbackExists(state.target.seed.ID) {
		response = *state.target.seed
	} else {
		if err := s.prewarmJSON(ctx, http.MethodPost, "/v1/playbacks", state.target.request, &response, true); err != nil {
			s.finishPlaybackPrewarm(key, state, CreatePlaybackResponse{}, err)
			return
		}
	}
	if !s.finishPlaybackPrewarm(key, state, response, nil) {
		if response.ID != "" {
			s.stopPreparedPlayback(response.ID)
		}
		return
	}
	bitmapSubtitleIndex := s.bitmapSubtitleIndexForPrewarm(
		ctx, response.ID, state.target.subtitleSelection,
	)
	s.prewarmMu.Lock()
	state.bitmapSubtitleIndex = bitmapSubtitleIndex
	claimed := state.claimed
	s.prewarmMu.Unlock()
	if claimed || ctx.Err() != nil {
		return
	}

	hlsRequest := map[string]any{"start_seconds": state.target.request.StartSeconds}
	if bitmapSubtitleIndex >= 0 {
		hlsRequest["bitmap_subtitle_index"] = bitmapSubtitleIndex
	}
	var stream any
	if err := s.prewarmJSON(
		ctx,
		http.MethodPost,
		fmt.Sprintf("/v1/playbacks/%s/hls", response.ID),
		hlsRequest,
		&stream,
		false,
	); err != nil {
		s.failReadyPlaybackPrewarm(key, state, err)
		return
	}

	s.prewarmMu.Lock()
	claimed = state.claimed
	s.prewarmMu.Unlock()
	if claimed || ctx.Err() != nil {
		return
	}
	parkContext, parkCancel := context.WithCancel(ctx)
	parkDone := make(chan struct{})
	s.prewarmMu.Lock()
	if state.claimed || s.prewarmStates[key] != state {
		s.prewarmMu.Unlock()
		parkCancel()
		return
	}
	state.parkCancel = parkCancel
	state.parkDone = parkDone
	s.prewarmMu.Unlock()
	var parked map[string]string
	err := s.prewarmJSON(
		parkContext,
		http.MethodPost,
		fmt.Sprintf("/v1/playbacks/%s/hls/park", response.ID),
		map[string]int{"buffer_seconds": prewarmBufferSeconds},
		&parked,
		false,
	)
	parkCancel()
	s.prewarmMu.Lock()
	state.parkCancel = nil
	state.parkDone = nil
	close(parkDone)
	claimed = state.claimed
	if err == nil && s.prewarmStates[key] == state && !claimed {
		state.bufferReady = true
		state.bufferedAt = time.Now()
	}
	s.prewarmMu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		s.failReadyPlaybackPrewarm(key, state, err)
		return
	}
	if err == nil {
		time.AfterFunc(prewarmHintTTL, func() { s.expireUnusedPrewarm(key, state) })
	}
}

func (s *Server) expireUnusedPrewarm(key string, state *playbackPrewarmState) {
	s.prewarmMu.Lock()
	if s.prewarmStates[key] != state || state.claimed {
		s.prewarmMu.Unlock()
		return
	}
	delete(s.prewarmStates, key)
	state.cancel()
	playbackID := state.response.ID
	s.prewarmMu.Unlock()
	if playbackID != "" {
		s.stopPreparedPlayback(playbackID)
	}
}

func (s *Server) finishPlaybackPrewarm(
	key string,
	state *playbackPrewarmState,
	response CreatePlaybackResponse,
	err error,
) bool {
	s.prewarmMu.Lock()
	if s.prewarmStates[key] != state {
		state.cancel()
		select {
		case <-state.playbackReady:
		default:
			close(state.playbackReady)
		}
		s.prewarmMu.Unlock()
		return false
	}
	state.response = response
	state.err = err
	select {
	case <-state.playbackReady:
	default:
		close(state.playbackReady)
	}
	if err != nil {
		delete(s.prewarmStates, key)
		state.cancel()
	}
	s.prewarmMu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("prewarm playback", "media_id", state.target.request.MediaID, "error", err)
	}
	return err == nil
}

func (s *Server) failReadyPlaybackPrewarm(key string, state *playbackPrewarmState, err error) {
	s.prewarmMu.Lock()
	claimed := state.claimed
	if s.prewarmStates[key] == state && !claimed {
		delete(s.prewarmStates, key)
		state.err = err
		state.cancel()
	}
	s.prewarmMu.Unlock()
	if claimed {
		return
	}
	if state.response.ID != "" {
		s.stopPreparedPlayback(state.response.ID)
	}
	if !errors.Is(err, context.Canceled) {
		s.logger.Warn("prewarm playback buffer", "media_id", state.target.request.MediaID, "error", err)
	}
}

func (s *Server) prewarmedPlaybackAvailable(state *playbackPrewarmState) bool {
	if state.response.ID == "" || !s.playbackExists(state.response.ID) {
		return false
	}
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		return false
	}
	languages := state.target.request.Preferences.Languages
	return manager.Prepared(
		state.response.ID,
		state.target.request.StartSeconds,
		languages,
		state.bitmapSubtitleIndex,
		prewarmBufferSeconds,
	)
}

func normalizedSubtitleSelection(
	selection playbackSubtitleSelection,
) (playbackSubtitleSelection, error) {
	mode := strings.ToLower(strings.TrimSpace(selection.Mode))
	switch mode {
	case "", "off":
		return playbackSubtitleSelection{Mode: "off"}, nil
	case "text":
		return playbackSubtitleSelection{Mode: "text"}, nil
	case "bitmap":
		if selection.Index < 0 {
			return playbackSubtitleSelection{}, errors.New("invalid bitmap subtitle selection")
		}
		selection.Mode = mode
		selection.Language = strings.ToLower(strings.TrimSpace(selection.Language))
		selection.Title = strings.TrimSpace(selection.Title)
		selection.Codec = strings.ToLower(strings.TrimSpace(selection.Codec))
		return selection, nil
	default:
		return playbackSubtitleSelection{}, fmt.Errorf("invalid subtitle selection mode %q", mode)
	}
}

func matchingBitmapSubtitleSelection(left, right playbackSubtitleSelection) bool {
	leftBitmap := strings.EqualFold(strings.TrimSpace(left.Mode), "bitmap")
	rightBitmap := strings.EqualFold(strings.TrimSpace(right.Mode), "bitmap")
	if !leftBitmap || !rightBitmap {
		return leftBitmap == rightBitmap
	}
	leftLanguage := normalizedSubtitleMetadata(left.Language)
	rightLanguage := normalizedSubtitleMetadata(right.Language)
	leftTitle := normalizedSubtitleMetadata(left.Title)
	rightTitle := normalizedSubtitleMetadata(right.Title)
	if leftLanguage != rightLanguage || leftTitle != rightTitle ||
		normalizedSubtitleMetadata(left.Codec) != normalizedSubtitleMetadata(right.Codec) ||
		left.Default != right.Default || left.Forced != right.Forced {
		return false
	}
	return leftLanguage != "" || leftTitle != "" || left.Index == right.Index
}

func (s *Server) bitmapSubtitleIndexForPrewarm(
	ctx context.Context,
	playbackID string,
	selection playbackSubtitleSelection,
) int {
	if !strings.EqualFold(strings.TrimSpace(selection.Mode), "bitmap") {
		return -1
	}
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		return -1
	}
	tracks, err := manager.ProbeSubtitles(ctx, playbackID)
	if err != nil {
		return -1
	}
	return matchingBitmapSubtitleIndex(tracks, selection)
}

func matchingBitmapSubtitleIndex(
	tracks []hls.SubtitleTrack,
	selection playbackSubtitleSelection,
) int {
	if !strings.EqualFold(strings.TrimSpace(selection.Mode), "bitmap") {
		return -1
	}
	language := normalizedSubtitleMetadata(selection.Language)
	title := normalizedSubtitleMetadata(selection.Title)
	codec := normalizedSubtitleMetadata(selection.Codec)
	languageFallback := -1
	for _, track := range tracks {
		if track.Kind != "bitmap" {
			continue
		}
		trackLanguage := normalizedSubtitleMetadata(track.Language)
		trackTitle := normalizedSubtitleMetadata(track.Title)
		trackCodec := normalizedSubtitleMetadata(track.Codec)
		if language != "" {
			if trackLanguage != language {
				continue
			}
			if title != "" && trackTitle == title {
				return track.Index
			}
			if languageFallback < 0 {
				languageFallback = track.Index
			}
			continue
		}
		if title != "" && trackTitle == title {
			return track.Index
		}
		if title == "" && track.Index == selection.Index &&
			(codec == "" || trackCodec == codec) {
			return track.Index
		}
	}
	return languageFallback
}

func normalizedSubtitleMetadata(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func (s *Server) prewarmJSON(
	ctx context.Context,
	method string,
	path string,
	input any,
	output any,
	internal bool,
) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	s.prewarmMu.Lock()
	baseURL := s.prewarmBaseURL
	client := s.prewarmClient
	s.prewarmMu.Unlock()
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if internal {
		request.Header.Set(prewarmRequestHeader, "1")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(contents, &payload)
		if payload.Error != "" {
			return errors.New(payload.Error)
		}
		return fmt.Errorf("prewarm request returned %s", response.Status)
	}
	if output == nil || len(contents) == 0 {
		return nil
	}
	if err := json.Unmarshal(contents, output); err != nil {
		return fmt.Errorf("decode prewarm response: %w", err)
	}
	return nil
}

func (s *Server) prewarmTargetForHistory(ctx context.Context, entry history.Entry) playbackPrewarmTarget {
	query := entry.Title
	if entry.SeriesTitle != "" {
		query = entry.SeriesTitle
	}
	return playbackPrewarmTarget{
		source: "history",
		request: CreatePlaybackRequest{
			MediaID: entry.MediaID, MediaType: entry.MediaType, Query: query, Year: entry.Year,
			SeriesID: entry.SeriesID, SeriesTitle: entry.SeriesTitle,
			SeasonNumber: entry.SeasonNumber, EpisodeNumber: entry.EpisodeNumber,
			EpisodeTitle: entry.EpisodeTitle, StartSeconds: entry.ResumePosition(),
			Preferences: catalog.Preferences{
				Languages:          s.originalAudioLanguages(ctx, entry.SeriesID),
				StreamingOptimized: true, PreferTextSubtitles: true,
			},
		},
	}
}

func (s *Server) originalAudioLanguages(ctx context.Context, seriesID string) []string {
	languages := []string{"en", "english"}
	if strings.TrimSpace(seriesID) == "" {
		return languages
	}
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.ShowProvider)
	s.metadataMu.RUnlock()
	if !ok {
		return languages
	}
	show, err := provider.Show(ctx, seriesID)
	language := strings.ToLower(strings.TrimSpace(show.OriginalLanguage))
	if err == nil && language != "" && language != "en" {
		languages = append([]string{language}, languages...)
	}
	return languages
}

func (s *Server) queueNextEpisodePrewarm(
	current history.Entry,
	subtitleSelection playbackSubtitleSelection,
) {
	s.prewarmMu.Lock()
	ctx := s.prewarmContext
	s.prewarmMu.Unlock()
	if ctx == nil {
		return
	}
	lookupContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if next, ok := s.nextEpisodeForPrewarming(lookupContext, current); ok {
		target := s.prewarmTargetForHistory(lookupContext, next)
		target.priority = true
		target.subtitleSelection = subtitleSelection
		s.queuePlaybackPrewarm(target)
	}
}

func (s *Server) nextEpisodeForPrewarming(ctx context.Context, current history.Entry) (history.Entry, bool) {
	if current.SeriesID == "" || current.SeasonNumber <= 0 || current.EpisodeNumber <= 0 {
		return history.Entry{}, false
	}
	s.metadataMu.RLock()
	provider, ok := s.metadataProvider.(metadata.ShowProvider)
	s.metadataMu.RUnlock()
	if !ok {
		return history.Entry{}, false
	}
	show, err := provider.Show(ctx, current.SeriesID)
	if err != nil {
		return history.Entry{}, false
	}
	for _, summary := range show.Seasons {
		if summary.Number < current.SeasonNumber {
			continue
		}
		season, err := provider.Season(ctx, show.ID, summary.Number)
		if err != nil {
			continue
		}
		for _, episode := range season.Episodes {
			if episode.SeasonNumber == current.SeasonNumber && episode.EpisodeNumber <= current.EpisodeNumber {
				continue
			}
			return history.Entry{
				MediaID: episode.ID, MediaType: string(metadata.MediaTypeShow),
				Title: show.Title, Year: show.Year, SeriesID: show.ID, SeriesTitle: show.Title,
				SeasonNumber: episode.SeasonNumber, EpisodeNumber: episode.EpisodeNumber,
				EpisodeTitle: episode.Title,
			}, true
		}
	}
	return history.Entry{}, false
}

func (s *Server) queuePlaybackHandoff(playbackID string) bool {
	s.mu.RLock()
	request, hasRequest := s.playbackRequests[playbackID]
	response, hasResponse := s.playbackResponses[playbackID]
	s.mu.RUnlock()
	if !hasRequest || !hasResponse || s.historyStore == nil || request.MediaType == string(metadata.MediaTypeShow) {
		return false
	}
	entries, err := s.historyStore.List()
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.MediaID != request.MediaID {
			continue
		}
		request.StartSeconds = entry.ResumePosition()
		target := playbackPrewarmTarget{
			request: request, source: "history", priority: true, seed: &response,
		}
		s.queuePlaybackPrewarm(target)
		return true
	}
	return false
}

func (s *Server) stopPreparedPlayback(playbackID string) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager != nil {
		manager.Stop(playbackID)
	}
}

func (s *Server) cancelPlaybackPrewarming() {
	s.prewarmMu.Lock()
	playbackIDs := make([]string, 0, len(s.prewarmStates))
	for key, state := range s.prewarmStates {
		delete(s.prewarmStates, key)
		state.cancel()
		if state.response.ID != "" {
			playbackIDs = append(playbackIDs, state.response.ID)
		}
	}
	s.prewarmMu.Unlock()
	for _, playbackID := range playbackIDs {
		s.stopPreparedPlayback(playbackID)
	}
}

func playbackPrewarmKey(request CreatePlaybackRequest) string {
	if mediaID := strings.TrimSpace(request.MediaID); mediaID != "" {
		return mediaID
	}
	if title := strings.ToLower(strings.Join(strings.Fields(request.Query), " ")); title != "" {
		return fmt.Sprintf("%s/%d/s%d/e%d", title, request.Year, request.SeasonNumber, request.EpisodeNumber)
	}
	return ""
}

func matchingPrewarmRequest(left, right CreatePlaybackRequest) bool {
	return playbackPrewarmKey(left) == playbackPrewarmKey(right) &&
		absFloat(left.StartSeconds-right.StartSeconds) <= 1 &&
		equalStringLists(left.Preferences.Languages, right.Preferences.Languages)
}

func matchingQueuedPrewarmTarget(left, right playbackPrewarmTarget) bool {
	return matchingPrewarmRequest(left.request, right.request) &&
		matchingBitmapSubtitleSelection(left.subtitleSelection, right.subtitleSelection)
}

func equalStringLists(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
