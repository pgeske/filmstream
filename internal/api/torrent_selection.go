package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/playbackcache"
	"github.com/pgeske/filmstream/internal/torrentstream"
)

// torrentSelectionPolicy bounds how a playback request turns ranked releases
// into one mounted torrent. Zero fields fall back to defaultTorrentSelection.
type torrentSelectionPolicy struct {
	// width is how many candidates are mounted concurrently.
	width int
	// maxAttempts caps the candidates mounted for one request.
	maxAttempts int
	// metadataWait bounds Create (torrent file download or magnet metadata).
	metadataWait time.Duration
	// livenessWait bounds the wait, after metadata, for connected peers and payload.
	livenessWait time.Duration
	// winnerGrace is how long a healthy lower-ranked candidate waits for
	// better-ranked candidates that are still starting.
	winnerGrace time.Duration
	// budget bounds the whole torrent path of a request: cache, search, and selection.
	budget time.Duration
	poll   time.Duration
}

var defaultTorrentSelection = torrentSelectionPolicy{
	width:        3,
	maxAttempts:  6,
	metadataWait: 20 * time.Second,
	livenessWait: 10 * time.Second,
	winnerGrace:  3 * time.Second,
	budget:       45 * time.Second,
	poll:         250 * time.Millisecond,
}

func (s *Server) selectionPolicy() torrentSelectionPolicy {
	policy := s.torrentSelection
	defaults := defaultTorrentSelection
	if policy.width <= 0 {
		policy.width = defaults.width
	}
	if policy.maxAttempts <= 0 {
		policy.maxAttempts = defaults.maxAttempts
	}
	if policy.metadataWait <= 0 {
		policy.metadataWait = defaults.metadataWait
	}
	if policy.livenessWait <= 0 {
		policy.livenessWait = defaults.livenessWait
	}
	if policy.winnerGrace <= 0 {
		policy.winnerGrace = defaults.winnerGrace
	}
	if policy.budget <= 0 {
		policy.budget = defaults.budget
	}
	if policy.poll <= 0 {
		policy.poll = defaults.poll
	}
	return policy
}

// playbackError carries the HTTP status a playback creation failure maps to.
type playbackError struct {
	status  int
	message string
}

func (e *playbackError) Error() string { return e.message }

type torrentPlaybackResult struct {
	session                    *torrentstream.Session
	selected                   *catalog.RankedCandidate
	releasePath                string
	playbackCacheHit           bool
	releaseSearchCacheHit      bool
	playbackCacheDuration      time.Duration
	releaseSearchCacheDuration time.Duration
	externalSearchDuration     time.Duration
	releaseSelectionDuration   time.Duration
}

// createTorrentPlayback selects and mounts a torrent for a search request
// within the selection budget: the cached known-good release first, then the
// best healthy candidates of the (possibly prefetched) ranking. Every candidate
// that fails is quarantined, so retries never pick it again.
func (s *Server) createTorrentPlayback(
	ctx context.Context,
	request CreatePlaybackRequest,
	prewarm bool,
) (torrentPlaybackResult, error) {
	var result torrentPlaybackResult
	ctx, cancel := context.WithTimeout(ctx, s.selectionPolicy().budget)
	defer cancel()
	scope := torrentFailureScope(request)

	cacheStarted := time.Now()
	session, selected, err := s.createCachedPlayback(ctx, request, prewarm)
	result.playbackCacheDuration = time.Since(cacheStarted)
	if err != nil {
		return result, err
	}
	if session != nil {
		result.session, result.selected = session, selected
		result.playbackCacheHit = true
		result.releasePath = "cached_torrent"
		return result, nil
	}

	lookupStarted := time.Now()
	ranked, found := s.cachedReleaseSearch(ctx, request)
	result.releaseSearchCacheDuration = time.Since(lookupStarted)
	search := !found
	if found {
		result.releaseSearchCacheHit = true
		s.logger.Info("reused prefetched release search", "media_id", request.MediaID, "candidates", len(ranked))
		if len(ranked) > 0 && len(s.availableTorrentCandidates(scope, ranked)) == 0 {
			// Every retained candidate failed recently; the retained ranking may
			// be stale, so search again at most once per cooldown.
			if !s.claimTorrentFreshSearch(scope) {
				return result, &playbackError{http.StatusBadGateway, fmt.Sprintf(
					"playback unavailable: all %d known releases failed recently; try again in a few minutes", len(ranked),
				)}
			}
			s.logger.Warn("all retained torrent candidates failed; forcing fresh release search",
				"media_id", request.MediaID, "candidates", len(ranked), "cooldown", s.torrentFreshSearchCooldown)
			search = true
		}
	}
	if search {
		searchStarted := time.Now()
		ranked, err = s.searchAndRank(ctx, s.releaseSearchRequest(ctx, request), request.OriginalTitle, catalog.ProtocolTorrent)
		result.externalSearchDuration = time.Since(searchStarted)
		if err != nil {
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return result, ctx.Err()
			}
			return result, &playbackError{http.StatusBadGateway, "release search failed: " + err.Error()}
		}
		s.cacheReleaseSearch(request, ranked)
	}
	if len(ranked) == 0 {
		return result, &playbackError{http.StatusNotFound, "no matching streaming candidates found"}
	}
	available := s.availableTorrentCandidates(scope, ranked)
	if len(available) == 0 {
		return result, &playbackError{http.StatusBadGateway, fmt.Sprintf(
			"playback unavailable: all %d matching releases failed recently; try again in a few minutes", len(ranked),
		)}
	}
	width := s.selectionPolicy().width
	if prewarm {
		if available, err = s.prewarmableTorrentCandidates(available); err != nil {
			return result, &playbackError{http.StatusConflict, err.Error()}
		}
		// A background prewarm must not compete with active playback for bandwidth.
		width = 1
	}

	selectionStarted := time.Now()
	session, selected, status, err := s.createRankedPlayback(ctx, available, scope, playbackFileHint(request), width)
	result.releaseSelectionDuration = time.Since(selectionStarted)
	if err != nil {
		return result, err
	}
	if prewarm && status.Private && !s.releaseInUse(selected.Candidate) && !s.headPrewarmAllowed(selected.Candidate) {
		// The indexer was not configured as private but the torrent is: reading
		// payload now would create a seeding obligation the user never asked for.
		_ = s.engine.Drop(session.ID)
		return result, &playbackError{http.StatusConflict, "the selected release is a private torrent no playback uses"}
	}
	result.session, result.selected = session, selected
	result.releasePath = "fresh_ranking"
	if found && !search {
		result.releasePath = "cached_ranking"
	}
	return result, nil
}

// createCachedPlayback mounts the release that last produced a working stream
// for this media (or its season), after the same liveness check as fresh
// candidates. A cached release that no longer starts is quarantined and
// removed so the caller falls through to a search.
func (s *Server) createCachedPlayback(
	ctx context.Context,
	request CreatePlaybackRequest,
	prewarm bool,
) (*torrentstream.Session, *catalog.RankedCandidate, error) {
	if s.playbackCache == nil {
		return nil, nil, nil
	}
	cacheMediaIDs := []string{request.MediaID}
	if seasonMediaID := seasonPlaybackCacheMediaID(request); seasonMediaID != "" && seasonMediaID != request.MediaID {
		cacheMediaIDs = append([]string{seasonMediaID}, cacheMediaIDs...)
	}
	var cached playbackcache.Entry
	cacheMediaID := ""
	for _, mediaID := range cacheMediaIDs {
		entry, found, err := s.playbackCache.Lookup(mediaID, request.Query, request.Year)
		if err != nil {
			s.logger.Warn("load cached playback selection", "title", request.Query, "error", err)
			return nil, nil, nil
		}
		if found {
			cached = entry
			cacheMediaID = mediaID
			break
		}
	}
	if cacheMediaID == "" {
		return nil, nil, nil
	}
	removeCached := func(reason string) {
		if err := s.playbackCache.Remove(cacheMediaID, request.Query, request.Year); err != nil {
			s.logger.Warn("remove cached playback selection", "title", request.Query, "reason", reason, "error", err)
		}
	}
	// Season-pack preference only chooses between candidates; it cannot change
	// eligibility when validating a single cached release.
	cachedSearch := catalog.SearchRequest{
		Query: request.Query, Year: request.Year, MediaType: request.MediaType,
		SeasonNumber: request.SeasonNumber, EpisodeNumber: request.EpisodeNumber,
		Preferences: request.Preferences,
	}
	if len(catalog.Rank(cachedSearch, []catalog.Candidate{cached.Selected.Candidate})) == 0 {
		removeCached("release policy")
		s.logger.Info("cached playback no longer matches release policy", "title", request.Query,
			"name", cached.Selected.Candidate.Name)
		return nil, nil, nil
	}
	scope := torrentFailureScope(request)
	if s.torrentCandidateRecentlyFailed(scope, cached.Selected.Candidate) {
		removeCached("quarantined")
		return nil, nil, nil
	}
	if prewarm && s.candidateIsPrivate(cached.Selected.Candidate) && !s.releaseInUse(cached.Selected.Candidate) &&
		!s.headPrewarmAllowed(cached.Selected.Candidate) {
		s.logger.Info("prewarm skips cached private release that no playback is using",
			"media_id", request.MediaID, "name", cached.Selected.Candidate.Name)
		return nil, nil, nil
	}

	race := s.raceTorrentCandidates(ctx, scope, []torrentAttempt{{
		candidate:   cached.Selected,
		torrentPath: cached.TorrentPath,
		fileHint:    playbackFileHint(request),
	}}, 1)
	if race.session == nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, ctx.Err()
		}
		if race.quarantined > 0 {
			removeCached("start failed")
		}
		s.logger.Warn("cached playback selection did not start; searching again", "title", request.Query,
			"name", cached.Selected.Candidate.Name, "error", strings.Join(race.failures, "; "))
		return nil, nil, nil
	}
	s.logger.Info("reused cached playback selection", "id", race.session.ID,
		"name", cached.Selected.Candidate.Name, "connected_peers", race.status.ActivePeers,
		"download_rate", race.status.DownloadRate)
	selected := cached.Selected
	return race.session, &selected, nil
}

// createRankedPlayback races the best available candidates and returns the
// best-ranked one whose swarm is serving data.
func (s *Server) createRankedPlayback(
	ctx context.Context,
	ranked []catalog.RankedCandidate,
	scope string,
	fileHint string,
	width int,
) (*torrentstream.Session, *catalog.RankedCandidate, torrentstream.Status, error) {
	attempts := make([]torrentAttempt, 0, len(ranked))
	for _, candidate := range ranked {
		if candidate.Candidate.Protocol == catalog.ProtocolUsenet || candidate.Candidate.NZBURL != "" {
			continue
		}
		attempts = append(attempts, torrentAttempt{candidate: candidate, fileHint: fileHint})
	}
	if len(attempts) == 0 {
		return nil, nil, torrentstream.Status{}, &playbackError{http.StatusNotFound, "no usable torrent candidates found"}
	}
	race := s.raceTorrentCandidates(ctx, scope, attempts, width)
	if race.session == nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, torrentstream.Status{}, ctx.Err()
		}
		message := fmt.Sprintf("no playable release found: %d of %d candidates tried", race.launched, len(attempts))
		if race.budgetExpired {
			message = fmt.Sprintf("no playable release found within %s: %d of %d candidates tried",
				s.selectionPolicy().budget, race.launched, len(attempts))
		}
		if len(race.failures) > 0 {
			message += " (" + strings.Join(race.failures, "; ") + ")"
		}
		return nil, nil, torrentstream.Status{}, &playbackError{http.StatusBadGateway, message}
	}
	return race.session, race.candidate, race.status, nil
}

type torrentAttempt struct {
	candidate catalog.RankedCandidate
	// torrentPath mounts a cached .torrent instead of resolving the candidate.
	torrentPath string
	fileHint    string
}

type swarmTier int

const (
	// swarmSilent: metadata is known but no peer is connected.
	swarmSilent swarmTier = iota
	// swarmConnected: peers are connected but no payload has arrived yet.
	swarmConnected
	// swarmFlowing: payload is arriving, or the selected file is already local.
	swarmFlowing
)

func (tier swarmTier) String() string {
	switch tier {
	case swarmFlowing:
		return "flowing"
	case swarmConnected:
		return "connected"
	default:
		return "silent"
	}
}

func torrentSwarmTier(status torrentstream.Status) swarmTier {
	if status.FileSize > 0 && status.BytesComplete >= status.FileSize {
		return swarmFlowing
	}
	if status.ActivePeers <= 0 && status.ConnectedSeeders <= 0 {
		return swarmSilent
	}
	if status.DownloadRate > 0 || status.BytesComplete > 0 {
		return swarmFlowing
	}
	return swarmConnected
}

type attemptUpdate struct {
	index   int
	session *torrentstream.Session
	status  torrentstream.Status
	tier    swarmTier
	err     error
	// final marks the attempt's last update.
	final bool
	// quarantine marks err as a failure of the candidate itself rather than of
	// the race being abandoned.
	quarantine bool
}

type torrentRaceResult struct {
	session       *torrentstream.Session
	candidate     *catalog.RankedCandidate
	status        torrentstream.Status
	failures      []string
	launched      int
	quarantined   int
	budgetExpired bool
}

// raceTorrentCandidates mounts up to width candidates at a time, in rank order,
// and picks the best-ranked one whose swarm serves data. A lower-ranked healthy
// candidate wins once every better one has finished, or after winnerGrace.
// When no candidate is serving data by the time all finish (or the budget
// ends), the best-ranked candidate with connected peers is used. Losing
// sessions are dropped before returning; they never served payload, so they
// leave no seeding obligation. Candidates that failed on their own (no
// metadata, no peers, mount error) are quarantined for the scope.
func (s *Server) raceTorrentCandidates(
	ctx context.Context,
	scope string,
	attempts []torrentAttempt,
	width int,
) torrentRaceResult {
	policy := s.selectionPolicy()
	started := time.Now()
	raceContext, cancelRace := context.WithCancel(ctx)
	defer cancelRace()

	type attemptState struct {
		launched   bool
		final      bool
		failed     bool
		quarantine bool
		tier       swarmTier
		session    *torrentstream.Session
		status     torrentstream.Status
		err        error
		startedAt  time.Time
		finishedAt time.Time
	}
	states := make([]attemptState, len(attempts))
	updates := make(chan attemptUpdate, len(attempts))
	var attemptsDone sync.WaitGroup
	next, inFlight := 0, 0
	limit := min(len(attempts), policy.maxAttempts)
	launch := func() {
		for inFlight < width && next < limit {
			states[next].launched = true
			states[next].startedAt = time.Now()
			attemptsDone.Add(1)
			go func(index int) {
				defer attemptsDone.Done()
				s.runTorrentAttempt(raceContext, index, attempts[index], policy, updates)
			}(next)
			next++
			inFlight++
		}
	}
	apply := func(update attemptUpdate) {
		state := &states[update.index]
		if update.session != nil {
			state.session = update.session
			state.status = update.status
			state.tier = update.tier
		}
		if update.err != nil {
			state.failed = true
			state.err = update.err
			state.quarantine = update.quarantine
		}
		if update.final && !state.final {
			state.final = true
			state.finishedAt = time.Now()
			inFlight--
		}
	}
	bestWith := func(tier swarmTier) int {
		for index := range states {
			if states[index].launched && !states[index].failed && states[index].session != nil && states[index].tier == tier {
				return index
			}
		}
		return -1
	}

	winner := -1
	var grace <-chan time.Time
	graceExpired := false
	budgetExpired := false
decide:
	for {
		if best := bestWith(swarmFlowing); best >= 0 {
			betterSettled := true
			for index := range best {
				betterSettled = betterSettled && states[index].final
			}
			if betterSettled || graceExpired {
				winner = best
				break decide
			}
			if grace == nil {
				grace = time.After(policy.winnerGrace)
			}
		} else {
			// Mount further candidates only while none serves data.
			launch()
		}
		if inFlight == 0 {
			winner = bestWith(swarmConnected)
			break decide
		}
		select {
		case update := <-updates:
			apply(update)
		case <-grace:
			graceExpired = true
		case <-ctx.Done():
			// The budget ran out: use the best candidate so far. A canceled
			// request (the client left) keeps nothing.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				budgetExpired = true
				winner = bestWith(swarmFlowing)
				if winner < 0 {
					winner = bestWith(swarmConnected)
				}
			}
			break decide
		}
	}
	// Stop the remaining attempts and collect their last updates, including
	// sessions mounted just before the cancellation, so none is leaked.
	cancelRace()
	go func() {
		attemptsDone.Wait()
		close(updates)
	}()
	for update := range updates {
		apply(update)
	}

	result := torrentRaceResult{launched: next, budgetExpired: budgetExpired}
	for index := range states {
		state := &states[index]
		if !state.launched {
			continue
		}
		candidate := attempts[index].candidate.Candidate
		if index == winner {
			result.session = state.session
			result.status = state.status
			selected := attempts[index].candidate
			result.candidate = &selected
			continue
		}
		if state.session != nil {
			if err := s.engine.Drop(state.session.ID); err != nil {
				s.logger.Warn("drop losing torrent candidate", "id", state.session.ID, "error", err)
			}
		}
		// Attempts stopped because the race ended say nothing about their release.
		if !state.failed || !state.quarantine {
			continue
		}
		result.failures = append(result.failures, fmt.Sprintf("%s: %v", candidate.Name, state.err))
		if candidate.InfoHash == "" {
			candidate.InfoHash = strings.ToLower(state.status.InfoHash)
		}
		s.markTorrentCandidateFailed(scope, candidate)
		result.quarantined++
		s.logger.Info("torrent candidate failed", "scope", scope, "rank", index+1,
			"name", candidate.Name, "indexer", candidate.Indexer,
			"connected_peers", state.status.ActivePeers, "error", state.err,
			"duration", state.finishedAt.Sub(state.startedAt))
	}

	winnerRank, winnerTier := 0, ""
	if winner >= 0 {
		winnerRank, winnerTier = winner+1, states[winner].tier.String()
	}
	s.logger.Info("release selection stages", "scope", scope, "candidates", len(attempts),
		"mount_attempts", result.launched, "quarantined_candidates", result.quarantined,
		"selected_rank", winnerRank, "selected_swarm", winnerTier,
		"connected_peers", result.status.ActivePeers, "download_rate", result.status.DownloadRate,
		"grace_expired", graceExpired, "budget_expired", budgetExpired,
		"total_duration", time.Since(started))
	if result.candidate != nil {
		reportedSeeders := -1
		if result.candidate.Candidate.Seeders != nil {
			reportedSeeders = *result.candidate.Candidate.Seeders
		}
		s.logger.Info("selected torrent release", "id", result.session.ID, "scope", scope,
			"rank", winnerRank, "name", result.candidate.Candidate.Name,
			"indexer", result.candidate.Candidate.Indexer, "reported_seeders", reportedSeeders)
	}
	return result
}

// runTorrentAttempt mounts one candidate and reports its swarm tier until the
// swarm serves data, the liveness window closes, or the race is abandoned.
func (s *Server) runTorrentAttempt(
	ctx context.Context,
	index int,
	attempt torrentAttempt,
	policy torrentSelectionPolicy,
	updates chan<- attemptUpdate,
) {
	source, err := s.torrentAttemptSource(ctx, attempt)
	if err != nil {
		updates <- attemptUpdate{index: index, err: err, final: true, quarantine: ctx.Err() == nil}
		return
	}
	createContext, cancel := context.WithTimeout(ctx, policy.metadataWait)
	session, err := s.engine.Create(createContext, source)
	metadataTimedOut := errors.Is(createContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	if err != nil {
		if metadataTimedOut {
			err = fmt.Errorf("no torrent metadata within %s", policy.metadataWait)
		}
		updates <- attemptUpdate{index: index, err: err, final: true, quarantine: ctx.Err() == nil}
		return
	}

	liveness := time.NewTimer(policy.livenessWait)
	defer liveness.Stop()
	poll := time.NewTicker(policy.poll)
	defer poll.Stop()
	reported := swarmTier(-1)
	for {
		status, ok := s.engine.Status(session.ID)
		if !ok {
			updates <- attemptUpdate{
				index: index, err: errors.New("torrent session disappeared after mount"),
				final: true, quarantine: ctx.Err() == nil,
			}
			return
		}
		tier := torrentSwarmTier(status)
		update := attemptUpdate{index: index, session: session, status: status, tier: tier}
		if tier == swarmFlowing {
			update.final = true
			updates <- update
			return
		}
		if tier != reported {
			updates <- update
			reported = tier
		}
		select {
		case <-ctx.Done():
			update.final = true
			updates <- update
			return
		case <-liveness.C:
			update.final = true
			if tier == swarmSilent {
				update.err = fmt.Errorf("no connected peers within %s of metadata", policy.livenessWait)
				update.quarantine = true
				if status.TrackerMessage != "" {
					update.err = fmt.Errorf("%w; tracker: %s", update.err, status.TrackerMessage)
				}
			}
			updates <- update
			return
		case <-poll.C:
		}
	}
}

func (s *Server) torrentAttemptSource(ctx context.Context, attempt torrentAttempt) (torrentstream.Source, error) {
	candidate := attempt.candidate.Candidate
	if attempt.torrentPath != "" {
		return torrentstream.Source{
			TorrentPath: attempt.torrentPath, FileHint: attempt.fileHint, Indexer: candidate.Indexer,
		}, nil
	}
	resolved, err := s.indexers.Resolve(ctx, candidate)
	if err != nil {
		return torrentstream.Source{}, err
	}
	return torrentstream.Source{
		MagnetURI: resolved.MagnetURI, TorrentURL: resolved.TorrentURL,
		FileHint: attempt.fileHint, Indexer: candidate.Indexer,
	}, nil
}

// prewarmableTorrentCandidates keeps the candidates a background prewarm may
// mount without creating a private-tracker obligation the user never asked
// for: public releases, releases an active playback already uses, and large
// releases on private trackers that allow head prewarm (the engine downloads
// only an unplayed private torrent's file head and tail). When the best
// candidate is any other private release, the prewarm is skipped instead of
// substituting a lower-ranked public release for the user's real choice.
func (s *Server) prewarmableTorrentCandidates(ranked []catalog.RankedCandidate) ([]catalog.RankedCandidate, error) {
	allowed := make([]catalog.RankedCandidate, 0, len(ranked))
	for index, candidate := range ranked {
		if !s.candidateIsPrivate(candidate.Candidate) || s.releaseInUse(candidate.Candidate) ||
			s.headPrewarmAllowed(candidate.Candidate) {
			allowed = append(allowed, candidate)
			continue
		}
		if index == 0 {
			return nil, fmt.Errorf("the best release %q is on private tracker %s and no playback uses it", candidate.Candidate.Name, candidate.Candidate.Indexer)
		}
	}
	return allowed, nil
}

// headPrewarmMinBytes keeps a prewarmed head and tail (a few 16 MiB pieces)
// a small fraction of the release, far below hit-and-run thresholds.
const headPrewarmMinBytes = 2 << 30

// headPrewarmAllowed reports whether a private release may be mounted by a
// prewarm that fetches only its file head and tail.
func (s *Server) headPrewarmAllowed(candidate catalog.Candidate) bool {
	return s.indexers != nil && s.indexers.HeadPrewarm(candidate.Indexer) &&
		candidate.SizeBytes >= headPrewarmMinBytes
}

func (s *Server) candidateIsPrivate(candidate catalog.Candidate) bool {
	return candidate.Private || s.indexers != nil && s.indexers.Private(candidate.Indexer)
}

// releaseInUse reports whether a live playback already streams the release,
// in which case another session on it creates no new seeding obligation.
func (s *Server) releaseInUse(candidate catalog.Candidate) bool {
	key := candidate.Key()
	indexerKey := candidate
	indexerKey.InfoHash = ""
	s.mu.RLock()
	var playbackIDs []string
	for playbackID, selected := range s.selected {
		if selected.Candidate.Key() == key || selected.Candidate.Indexer != "" && indexerKey.Key() == withoutInfoHash(selected.Candidate).Key() {
			playbackIDs = append(playbackIDs, playbackID)
		}
	}
	s.mu.RUnlock()
	for _, playbackID := range playbackIDs {
		if _, ok := s.engine.Get(playbackID); ok {
			return true
		}
	}
	return false
}

func withoutInfoHash(candidate catalog.Candidate) catalog.Candidate {
	candidate.InfoHash = ""
	return candidate
}

// torrentFailureScope names the media a quarantine applies to: a release that
// failed for one episode may still serve another from the same pack.
func torrentFailureScope(request CreatePlaybackRequest) string {
	if mediaID := strings.ToLower(strings.TrimSpace(request.MediaID)); mediaID != "" {
		return mediaID
	}
	return fmt.Sprintf("query:%s:%d:s%de%d", strings.ToLower(strings.TrimSpace(request.Query)),
		request.Year, request.SeasonNumber, request.EpisodeNumber)
}

// torrentFailureKeys lists the quarantine keys of a candidate: its info hash
// (shared by every indexer listing the same torrent) and its indexer-scoped ID.
func torrentFailureKeys(scope string, candidate catalog.Candidate) []string {
	prefix := strings.ToLower(strings.TrimSpace(scope)) + "\x00"
	keys := []string{prefix + withoutInfoHash(candidate).Key()}
	if candidate.InfoHash != "" {
		keys = append(keys, prefix+candidate.Key())
	}
	return keys
}

func (s *Server) markTorrentCandidateFailed(scope string, candidate catalog.Candidate) {
	if strings.TrimSpace(scope) == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.torrentFailures == nil {
		s.torrentFailures = make(map[string]time.Time)
	}
	for key, expiresAt := range s.torrentFailures {
		if now.After(expiresAt) {
			delete(s.torrentFailures, key)
		}
	}
	for _, key := range torrentFailureKeys(scope, candidate) {
		s.torrentFailures[key] = now.Add(torrentUnavailableFailureTTL)
	}
}

func (s *Server) torrentCandidateRecentlyFailed(scope string, candidate catalog.Candidate) bool {
	if strings.TrimSpace(scope) == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	failed := false
	for _, key := range torrentFailureKeys(scope, candidate) {
		expiresAt, found := s.torrentFailures[key]
		if found && now.After(expiresAt) {
			delete(s.torrentFailures, key)
			continue
		}
		failed = failed || found
	}
	return failed
}

func (s *Server) availableTorrentCandidates(scope string, ranked []catalog.RankedCandidate) []catalog.RankedCandidate {
	available := make([]catalog.RankedCandidate, 0, len(ranked))
	for _, candidate := range ranked {
		if candidate.Candidate.Protocol == catalog.ProtocolUsenet || candidate.Candidate.NZBURL != "" {
			continue
		}
		if s.torrentCandidateRecentlyFailed(scope, candidate.Candidate) {
			continue
		}
		available = append(available, candidate)
	}
	return available
}

// claimTorrentFreshSearch reports whether a forced fresh release search may run
// for the scope now, and starts its cooldown. A zero cooldown disables it.
func (s *Server) claimTorrentFreshSearch(scope string) bool {
	if s.torrentFreshSearchCooldown <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, found := s.torrentFreshSearchAt[scope]; found && time.Since(last) < s.torrentFreshSearchCooldown {
		return false
	}
	if s.torrentFreshSearchAt == nil {
		s.torrentFreshSearchAt = make(map[string]time.Time)
	}
	s.torrentFreshSearchAt[scope] = time.Now()
	return true
}

// clearSuccessfulTorrentRecovery resets the fresh-search cooldown once a
// torrent playback starts, so a later failure may search again immediately.
func (s *Server) clearSuccessfulTorrentRecovery(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.playbackCacheKeys[id]
	if !ok || key.source != catalog.ProtocolTorrent {
		return
	}
	delete(s.torrentFreshSearchAt, torrentFailureScope(s.playbackRequests[id]))
}

// releaseFailure reports whether a failed HLS start or media probe blames the
// selected release: any failure except the playback being stopped.
func releaseFailure(causes []error) bool {
	failed := false
	for _, cause := range causes {
		if cause == nil {
			continue
		}
		if errors.Is(cause, context.Canceled) {
			return false
		}
		failed = true
	}
	return failed
}
