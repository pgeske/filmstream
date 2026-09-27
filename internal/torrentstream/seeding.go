package torrentstream

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pgeske/filmstream/internal/config"
)

// privateObligationFraction is the share of a private torrent that may be
// downloaded before Filmstream treats it as a snatch that must be completed
// and seeded, even if it was never played. It stays well below the 10% at
// which trackers such as TorrentLeech start counting a hit-and-run.
const privateObligationFraction = 0.05

// defaultPrivateRule applies to private torrents from trackers without a
// configured or known rule.
var defaultPrivateRule = config.SeedRule{Ratio: 1, Hours: 240}

// knownTrackerRules are the published hit-and-run rules of private trackers,
// matched against the indexer name.
var knownTrackerRules = map[string]config.SeedRule{
	"torrentleech": {Ratio: 1, Hours: 240},
	"avistaz":      {Hours: 72, HoursPerGiB: 2},
}

func indexerKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func knownTrackerRule(key string) (config.SeedRule, bool) {
	for name, rule := range knownTrackerRules {
		if key != "" && strings.Contains(key, name) {
			return rule, true
		}
	}
	return config.SeedRule{}, false
}

// SetIndexers replaces the indexer settings that decide which torrents are
// private and which seed rule binds them. Torrents already playing keep the
// rule recorded when they were added.
func (e *Engine) SetIndexers(indexers []config.Indexer) {
	byName := make(map[string]config.Indexer, len(indexers))
	for _, indexer := range indexers {
		byName[indexerKey(indexer.Name)] = indexer
	}
	e.indexers.Store(&byName)
}

// seedPolicy decides whether a torrent is private and which seed rule binds
// it: the indexer's configured rule, a known tracker's rule, or the default.
func (e *Engine) seedPolicy(indexerName string, flagged bool) (bool, *config.SeedRule) {
	key := indexerKey(indexerName)
	var indexer config.Indexer
	configured := false
	if byName := e.indexers.Load(); byName != nil {
		indexer, configured = (*byName)[key]
	}
	known, isKnown := knownTrackerRule(key)
	if !flagged && !(configured && indexer.Private) && !isKnown {
		return false, nil
	}
	rule := defaultPrivateRule
	switch {
	case configured && indexer.Seed != nil:
		rule = *indexer.Seed
	case isKnown:
		rule = known
	}
	return true, &rule
}

// seedState is a torrent's progress towards its seeding requirement.
type seedState struct {
	private     bool
	complete    bool
	ratio       float64
	ratioTarget float64
	ratioMet    bool
	// seeded counts only time spent seeding a complete torrent (private) or
	// holding the finished selection (public).
	seeded   time.Duration
	required time.Duration
	met      bool
}

func (e *Engine) seedState(record managedTorrent, detail pluginTorrent) seedState {
	state := seedState{
		private:  record.Private || detail.Private,
		complete: detail.TotalSize > 0 && (detail.IsSeeding || detail.TotalDone >= detail.TotalSize),
		ratio:    transferRatio(detail.AllTimeDownload, detail.AllTimeUpload),
	}
	if state.private {
		rule := e.recordRule(record)
		state.ratioTarget = rule.Ratio
		state.ratioMet = rule.Ratio > 0 && detail.AllTimeDownload > 0 && state.ratio >= rule.Ratio
		state.seeded = time.Duration(detail.SeedingSeconds) * time.Second
		state.required = requiredSeedTime(rule, detail.TotalSize)
		timeMet := state.required > 0 && state.seeded >= state.required
		noRule := rule.Ratio <= 0 && state.required <= 0
		state.met = state.complete && (state.ratioMet || timeMet || noRule)
		return state
	}
	state.ratioTarget = e.seedRatioTarget
	state.ratioMet = ratioTargetMet(detail.AllTimeDownload, state.ratio, e.seedRatioTarget)
	state.seeded = time.Duration(max(detail.SeedingSeconds, detail.FinishedSeconds)) * time.Second
	state.required = e.seedMaxAge
	state.met = state.ratioMet || state.seeded >= state.required
	return state
}

func (e *Engine) recordRule(record managedTorrent) config.SeedRule {
	if record.Seed != nil {
		return *record.Seed
	}
	_, rule := e.seedPolicy(record.Indexer, true)
	return *rule
}

// remaining reports how much longer the torrent must seed when that follows
// from elapsed time alone.
func (s seedState) remaining() (time.Duration, bool) {
	if s.met {
		return 0, true
	}
	if s.private && !s.complete || s.required <= 0 {
		return 0, false
	}
	return max(0, s.required-s.seeded), true
}

func requiredSeedTime(rule config.SeedRule, totalSize int64) time.Duration {
	hours := rule.Hours + rule.HoursPerGiB*gib(totalSize)
	return time.Duration(hours * float64(time.Hour))
}

func obligationReached(detail pluginTorrent) bool {
	return detail.TotalSize > 0 && float64(detail.AllTimeDownload) >= privateObligationFraction*float64(detail.TotalSize)
}

func transferRatio(downloaded, uploaded int64) float64 {
	if downloaded <= 0 {
		return 0
	}
	return float64(uploaded) / float64(downloaded)
}

func ratioTargetMet(downloaded int64, ratio, target float64) bool {
	return target <= 0 || downloaded > 0 && ratio >= target
}

func (e *Engine) runJanitor(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cleanupInterval)
	defer ticker.Stop()
	for {
		e.maintain(ctx, time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// maintain reconciles records with Deluge and retires torrents whose
// seeding lifecycle is over.
func (e *Engine) maintain(ctx context.Context, now time.Time) {
	listing, err := e.plugin.list(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.Warn("could not list Deluge torrents", "error", err)
		}
		return
	}
	present := make(map[string]pluginTorrent, len(listing))
	for _, detail := range listing {
		present[strings.ToLower(detail.InfoHash)] = detail
	}
	e.reconcile(ctx, present)
	e.retire(ctx, present, now)
}

// reconcile keeps Deluge in line with the records: it re-adds torrents Deluge
// lost, requests every file of private obligations, and obligates private
// torrents that downloaded enough to count as a snatch.
func (e *Engine) reconcile(ctx context.Context, present map[string]pluginTorrent) {
	var missing, pushes, obligations []*torrentState
	e.mu.Lock()
	changed := false
	tracked := make([]*torrentState, 0, len(e.torrents))
	for _, t := range e.torrents {
		record := t.record
		if record == nil || t.pending > 0 {
			continue
		}
		detail, ok := present[t.hash]
		if !ok {
			missing = append(missing, t)
			continue
		}
		tracked = append(tracked, t)
		if detail.Private && !record.Private {
			record.Private = true
			changed = true
		}
		if record.Private && record.Seed == nil {
			_, record.Seed = e.seedPolicy(record.Indexer, true)
			changed = true
		}
		switch {
		case record.Private && !record.Started && !record.Obligated && obligationReached(detail):
			obligations = append(obligations, t)
		case record.WantAll && !t.wantAllPushed:
			pushes = append(pushes, t)
		}
	}
	if changed {
		e.persistLocked()
	}
	e.mu.Unlock()
	for _, t := range tracked {
		t.storeSummary(present[t.hash])
		e.backfillMetainfo(ctx, t, present[t.hash])
	}
	for _, t := range obligations {
		e.obligate(ctx, t, "downloaded_bytes", present[t.hash].AllTimeDownload)
	}
	for _, t := range pushes {
		e.pushWantAll(ctx, t)
	}
	for _, t := range missing {
		e.readd(ctx, t)
	}
}

// backfillMetainfo saves the metainfo of a tracked torrent that has none yet,
// for example a magnet whose first fetch failed, so readd can restore it.
func (e *Engine) backfillMetainfo(ctx context.Context, t *torrentState, detail pluginTorrent) {
	if !detail.HasMetadata {
		return
	}
	if _, err := os.Stat(e.managedMetainfoPath(t.hash)); !errors.Is(err, os.ErrNotExist) {
		return
	}
	contents, err := e.plugin.metainfo(ctx, t.hash)
	if err != nil {
		e.logger.Warn("could not save torrent metainfo", "info_hash", t.hash, "error", err)
		return
	}
	e.saveMetainfo(t.hash, contents)
}

// storeSummary records list-level status; it lacks file details, so it only
// feeds progress tracking and does not replace a cached detail.
func (t *torrentState) storeSummary(summary pluginTorrent) {
	t.detailMu.Lock()
	defer t.detailMu.Unlock()
	if t.lastProgressAt.IsZero() || summary.TotalDone > t.lastDone {
		t.lastDone = summary.TotalDone
		t.lastProgressAt = time.Now()
	}
}

// readd restores a recorded torrent Deluge no longer has from its saved
// metainfo. A record that cannot be restored is kept and retried.
func (e *Engine) readd(ctx context.Context, t *torrentState) {
	contents, readErr := os.ReadFile(e.managedMetainfoPath(t.hash))
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.mu.Lock()
	if e.torrents[t.hash] != t || t.record == nil {
		e.mu.Unlock()
		return
	}
	record := *t.record
	warned := e.missingWarned[t.hash]
	e.missingWarned[t.hash] = true
	e.mu.Unlock()
	if readErr != nil {
		if !warned {
			e.logger.Error("managed torrent is missing from Deluge and has no saved metainfo; keeping its record",
				"info_hash", t.hash, "name", record.Name, "private", record.Private, "error", readErr)
		}
		return
	}
	_, err := e.plugin.add(ctx, pluginAddRequest{
		Torrent: encodeTorrent(contents), SaveRoot: e.downloadsDir, WantedFiles: wantedFiles(record.WantAll, record.Files),
	})
	if err != nil {
		if !warned {
			e.logger.Warn("could not re-add managed torrent to Deluge; will retry", "info_hash", t.hash, "error", err)
		}
		return
	}
	e.mu.Lock()
	t.wantAllPushed = record.WantAll
	delete(e.missingWarned, t.hash)
	e.mu.Unlock()
	e.logger.Info("re-added managed torrent to Deluge", "info_hash", t.hash, "name", record.Name,
		"private", record.Private, "want_all", record.WantAll)
}

type retirement struct {
	t            *torrentState
	record       managedTorrent
	seed         seedState
	stored       int64
	lastActivity time.Time
	obligated    bool
}

// retire removes idle torrents whose lifecycle is over, then enforces the
// session and cache limits using only public or satisfied torrents. Active
// playback and unmet private obligations are never removed.
func (e *Engine) retire(ctx context.Context, present map[string]pluginTorrent, now time.Time) {
	e.mu.Lock()
	var idle []*retirement
	var stored int64
	count := 0
	for _, t := range e.torrents {
		if t.record == nil {
			continue
		}
		detail, ok := present[t.hash]
		count++
		stored += detail.TotalDone
		if !ok || t.pending > 0 {
			continue
		}
		active := false
		last := t.lastActivityLocked()
		for _, session := range t.sessions {
			active = active || session.activeStreams > 0
		}
		if active || now.Sub(last) < e.idleGrace {
			continue
		}
		record := *t.record
		idle = append(idle, &retirement{
			t: t, record: record, seed: e.seedState(record, detail), stored: detail.TotalDone, lastActivity: last,
			obligated: record.Started || record.Obligated || record.Private && obligationReached(detail),
		})
	}
	handler := e.onCleanup
	e.mu.Unlock()

	selected := make(map[*retirement]string)
	for _, candidate := range idle {
		switch {
		case candidate.seed.private && candidate.obligated:
			if candidate.seed.met {
				selected[candidate] = "seed-requirement"
			}
		case !candidate.obligated:
			selected[candidate] = "unused"
		case candidate.seed.ratioMet:
			selected[candidate] = "ratio-target"
		case candidate.seed.seeded >= e.seedMaxAge:
			selected[candidate] = "seed-time-target"
		}
	}
	remaining := count - len(selected)
	for candidate := range selected {
		stored -= candidate.stored
	}
	var evictable []*retirement
	for _, candidate := range idle {
		if _, done := selected[candidate]; !done && (!candidate.seed.private || candidate.seed.met) {
			evictable = append(evictable, candidate)
		}
	}
	sort.Slice(evictable, func(i, j int) bool {
		return evictable[i].lastActivity.Before(evictable[j].lastActivity)
	})
	for _, candidate := range evictable {
		switch {
		case remaining > e.maxSeedSessions:
			selected[candidate] = "session-limit"
		case stored > e.cacheLimitBytes:
			selected[candidate] = "cache-limit"
		default:
			continue
		}
		remaining--
		stored -= candidate.stored
	}

	for candidate, reason := range selected {
		wasObligated := candidate.obligated
		ids, removed := e.removeTorrent(ctx, candidate.t, reason, func(t *torrentState) bool {
			if t.record == nil || now.Sub(t.lastActivityLocked()) < e.idleGrace {
				return false
			}
			// A torrent that became an obligation since the snapshot is
			// re-evaluated on the next pass.
			return wasObligated || !t.record.Started && !t.record.Obligated
		})
		if !removed {
			continue
		}
		e.logger.Info("retired torrent", "reason", reason, "info_hash", candidate.t.hash, "name", candidate.record.Name,
			"private", candidate.seed.private, "stored_bytes", candidate.stored, "ratio", candidate.seed.ratio,
			"seeded", candidate.seed.seeded)
		if handler != nil {
			for _, id := range ids {
				handler(id, reason)
			}
		}
	}
}

// lastActivityLocked is the torrent's most recent playback activity. Callers
// hold Engine.mu.
func (t *torrentState) lastActivityLocked() time.Time {
	last := time.Time{}
	if t.record != nil {
		last = t.record.LastActivity
	}
	for _, session := range t.sessions {
		if session.lastActivity.After(last) {
			last = session.lastActivity
		}
	}
	return last
}
