// AVMediaSelectionGroup is not Sendable, although the main actor alone uses it.
@preconcurrency import AVFoundation
import Combine
import Foundation
#if os(iOS) || os(tvOS)
import MediaPlayer
#endif
import os

/// Drives one AVPlayer through a server HLS playback: installs items, watches
/// liveness, recovers stalls, and applies seeks and track changes that need the
/// server to prepare the stream again. Platform views observe its published state.
@MainActor
public final class NativePlaybackController: ObservableObject {
    public enum ItemOperation: Equatable, Sendable {
        case seeking
        case reloading
        case recovering
        case changingSubtitles
        case changingAudio

        var label: String {
            switch self {
            case .seeking: "Seeking…"
            case .reloading, .recovering: "Reconnecting…"
            case .changingSubtitles: "Changing Subtitles…"
            case .changingAudio: "Changing Audio…"
            }
        }
    }

    /// The server packages as fast as the source delivers, so AVPlayer may hold a
    /// deep buffer that rides out short torrent hiccups.
    public static let preferredForwardBufferDuration: TimeInterval = 30

    public let player = AVPlayer()

    @Published public private(set) var positionSeconds: Double
    @Published public private(set) var durationSeconds: Double
    @Published public private(set) var isPlaying = false
    @Published public private(set) var isWaiting = true
    @Published public private(set) var didReachEnd = false
    @Published public private(set) var stateLabel = "Preparing Stream…"
    /// Server-side view of a wait, for example "Connecting to peers… · 3 peers found".
    @Published public private(set) var waitingDetail: String?
    @Published public private(set) var errorMessage: String?
    @Published public private(set) var subtitleOptions: [HLSSubtitleTrack]
    @Published public private(set) var selectedSubtitle: HLSSubtitleTrack?
    /// Cue text for the overlay fallback of older servers; always nil when AVPlayer
    /// renders subtitle renditions itself.
    @Published public private(set) var activeSubtitleText: String?
    @Published public private(set) var audioOptions: [HLSAudioTrack]
    @Published public private(set) var selectedAudio: HLSAudioTrack?
    @Published public private(set) var operation: ItemOperation?

    public var isSeeking: Bool { operation == .seeking }
    public var playbackID: String { playback.id }
    /// True when text subtitles are native renditions, styled by the system's
    /// accessibility caption settings.
    public private(set) var usesNativeSubtitles: Bool

    private static let logger = Logger(subsystem: "com.alyoshukai.filmstream", category: "playback")
    private static var now: TimeInterval { ProcessInfo.processInfo.systemUptime }

    private let api: FilmstreamAPI
    private let movie: Movie
    private var playback: Playback
    private var timeline: HLSPlaybackTimeline
    private var streamURL: URL
    private var burnedSubtitleIndex: Int?
    private var packagedAudioStreamIndex: Int?
    private var wantsToPlay = false
    private var stopped = false
    private var interruptedWhilePlaying: Bool?
    private var liveness: PlaybackLiveness
    private var lastServerError: String?
    private weak var videoLayer: AVPlayerLayer?

    // One generation owns all item-changing work, including queued AVPlayer callbacks.
    private var operationGeneration = 0
    private var operationTask: Task<Void, Never>?
    private var pendingSeekSeconds: Double?
    private var watchdogTask: Task<Void, Never>?
    private var statusTask: Task<Void, Never>?

    // Identifies the installed AVPlayerItem for its observers.
    private var itemGeneration = 0
    private var itemObservations: [NSKeyValueObservation] = []
    private var itemObservers: [NSObjectProtocol] = []
    private var timeObserver: Any?
    private var playbackObservation: NSKeyValueObservation?
    private var subtitleTask: Task<Void, Never>?
    private var legibleGroup: AVMediaSelectionGroup?
    private var subtitleCues: [SubtitleCue] = []
    private var lastNowPlayingSecond = -1

    #if os(iOS) || os(tvOS)
    private static var activeMediaSessionID: String?
    private var remoteCommandTargets: [(MPRemoteCommand, Any)] = []
    private var mediaSessionID: String?
    #endif

    public init(movie: Movie, prepared: PreparedPlayback, api: FilmstreamAPI) {
        let hls = prepared.hls
        let timeline = hls.timeline
        let subtitles = hls.subtitles ?? []
        let audioTracks = hls.audioTracks ?? []
        self.api = api
        self.movie = movie
        playback = prepared.playback
        self.timeline = timeline
        streamURL = hls.streamURL
        usesNativeSubtitles = hls.masterURL != nil
        positionSeconds = timeline.requestedSeconds
        durationSeconds = max(0, hls.durationSeconds ?? 0)
        subtitleOptions = subtitles
        audioOptions = audioTracks
        burnedSubtitleIndex = hls.burnedSubtitleIndex
        packagedAudioStreamIndex = hls.audioStreamIndex
        selectedAudio = audioTracks.first { $0.index == hls.audioStreamIndex }
        selectedSubtitle = NativePlaybackController.initialSubtitle(
            in: subtitles,
            burnedIndex: hls.burnedSubtitleIndex
        )
        liveness = PlaybackLiveness(now: ProcessInfo.processInfo.systemUptime, playerSeconds: 0)

        player.automaticallyWaitsToMinimizeStalling = true
        player.actionAtItemEnd = .pause
        // Subtitle renditions follow the viewer's choice, not system auto-selection.
        player.appliesMediaSelectionCriteriaAutomatically = false
        installItem()
        let initialPlayerSeconds = timeline.playerSeconds(forMediaSeconds: timeline.requestedSeconds)
        if initialPlayerSeconds > 0 {
            player.seek(
                to: CMTime(seconds: initialPlayerSeconds, preferredTimescale: 600),
                toleranceBefore: .zero,
                toleranceAfter: .zero
            )
        }

        timeObserver = player.addPeriodicTimeObserver(
            forInterval: CMTime(seconds: 0.25, preferredTimescale: 600),
            queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated {
                self?.playerTimeAdvanced()
            }
        }
        playbackObservation = player.observe(\.timeControlStatus, options: [.initial, .new]) { [weak self] player, _ in
            let status = player.timeControlStatus
            let reason = player.reasonForWaitingToPlay?.rawValue
            Task { @MainActor in
                self?.timeControlStatusChanged(status, waitingReason: reason)
            }
        }
    }

    // MARK: - Transport

    public func attachVideoLayer(_ layer: AVPlayerLayer) {
        videoLayer = layer
    }

    public func play() {
        guard !stopped else { return }
        if errorMessage != nil {
            retry()
            return
        }
        wantsToPlay = true
        isPlaying = true
        didReachEnd = false
        if operation == nil {
            player.play()
            refreshWaitingState()
        }
        startWatchdog()
        publishNowPlayingInfo()
    }

    public func pause() {
        guard !stopped else { return }
        wantsToPlay = false
        isPlaying = false
        stopWatchdog()
        player.pause()
        refreshWaitingState()
        publishNowPlayingInfo()
    }

    public func togglePlayback() {
        if wantsToPlay {
            pause()
        } else {
            play()
        }
    }

    /// Clears a playback error and asks the server to prepare the stream again at
    /// the current position.
    public func retry() {
        guard !stopped, errorMessage != nil else { return }
        errorMessage = nil
        liveness = PlaybackLiveness(now: Self.now, playerSeconds: currentPlayerSeconds)
        wantsToPlay = true
        isPlaying = true
        startRecovery()
        startWatchdog()
    }

    public func jump(by seconds: Double) {
        guard !stopped, durationSeconds > 0 else { return }
        let origin = pendingSeekSeconds ?? positionSeconds
        seek(to: origin + seconds)
    }

    public func seek(to requestedSeconds: Double) {
        guard !stopped, durationSeconds > 0 else { return }
        let target = min(max(0, requestedSeconds), max(0, durationSeconds - 1))
        guard errorMessage == nil else {
            // Retry resumes from here.
            positionSeconds = target
            publishNowPlayingInfo()
            return
        }
        let generation = beginItemOperation(.seeking)
        pendingSeekSeconds = target
        positionSeconds = target
        operationTask = Task { @MainActor [weak self] in
            do {
                try await Task.sleep(for: .milliseconds(450))
            } catch {
                return
            }
            await self?.performSeek(to: target, generation: generation)
        }
    }

    /// The scene went inactive or to the background: pause, remembering intent.
    public func interrupt() {
        guard !stopped else { return }
        if interruptedWhilePlaying == nil {
            interruptedWhilePlaying = wantsToPlay
        }
        pause()
    }

    /// The scene is active again. The existing item is kept; if it failed meanwhile,
    /// the liveness watchdog replaces it once playback resumes.
    public func resumeAfterInterruption() {
        guard !stopped, let resume = interruptedWhilePlaying else { return }
        interruptedWhilePlaying = nil
        if resume {
            play()
        }
    }

    public func stop() {
        guard !stopped else { return }
        stopped = true
        cancelOperation()
        stopWatchdog()
        subtitleTask?.cancel()
        subtitleTask = nil
        deactivateMediaSession()
        removeItemObservers()
        player.pause()
        player.isMuted = true
        player.replaceCurrentItem(with: nil)
        if let timeObserver {
            player.removeTimeObserver(timeObserver)
            self.timeObserver = nil
        }
        playbackObservation?.invalidate()
        playbackObservation = nil
    }

    // MARK: - Tracks

    public func selectSubtitle(_ track: HLSSubtitleTrack?) {
        guard !stopped else { return }
        selectedSubtitle = track
        HLSSubtitleTrack.savePreference(track)
        if desiredBitmapIndex != burnedSubtitleIndex || operation == .changingSubtitles {
            // Bitmap subtitles exist only burned into the video.
            reprepare(.changingSubtitles, at: max(0, positionSeconds))
        } else if usesNativeSubtitles {
            applyNativeSubtitleSelection()
        } else {
            restartSubtitleUpdates()
        }
    }

    /// The packaged stream carries one audio track, so switching prepares it again.
    public func selectAudio(_ track: HLSAudioTrack) {
        guard !stopped, track.index != selectedAudio?.index else { return }
        selectedAudio = track
        guard track.index != packagedAudioStreamIndex || operation == .changingAudio else { return }
        reprepare(.changingAudio, at: max(0, positionSeconds))
    }

    private var desiredAudioIndex: Int? {
        selectedAudio?.index ?? packagedAudioStreamIndex
    }

    private var desiredBitmapIndex: Int? {
        guard let selectedSubtitle, selectedSubtitle.isBitmap else { return nil }
        return selectedSubtitle.index
    }

    private static func initialSubtitle(in tracks: [HLSSubtitleTrack], burnedIndex: Int?) -> HLSSubtitleTrack? {
        if let burnedIndex {
            return tracks.first { $0.index == burnedIndex }
        }
        let saved = HLSSubtitleTrack.savedPreference(in: tracks)
        return saved?.isBitmap == true ? nil : saved
    }

    // MARK: - Item operations

    private func beginItemOperation(_ newOperation: ItemOperation) -> Int {
        cancelOperation()
        operation = newOperation
        player.pause()
        isWaiting = true
        stateLabel = newOperation.label
        waitingDetail = nil
        errorMessage = nil
        return operationGeneration
    }

    private func cancelOperation() {
        operationGeneration += 1
        operationTask?.cancel()
        operationTask = nil
        operation = nil
        pendingSeekSeconds = nil
        player.currentItem?.cancelPendingSeeks()
    }

    private func performSeek(to target: Double, generation: Int) async {
        guard generation == operationGeneration, !stopped else { return }
        let playerTarget = timeline.playerSeconds(forMediaSeconds: target)
        if canSeekLocally(to: playerTarget) {
            player.seek(
                to: CMTime(seconds: playerTarget, preferredTimescale: 600),
                toleranceBefore: .zero,
                toleranceAfter: .zero
            ) { [weak self] finished in
                Task { @MainActor in
                    self?.finishOperation(generation: generation, finished: finished)
                }
            }
            return
        }
        await performReprepare(at: target, generation: generation)
    }

    private func canSeekLocally(to seconds: Double) -> Bool {
        guard seconds >= 0, let item = player.currentItem, item.status != .failed else { return false }
        let target = CMTime(seconds: seconds, preferredTimescale: 600)
        return item.seekableTimeRanges.contains { value in
            CMTimeRangeContainsTime(value.timeRangeValue, time: target)
        }
    }

    /// First remedy for a stall: a fresh item for the same stream, no server request.
    private func reloadItem() {
        guard !stopped else { return }
        let mediaSeconds = positionSeconds
        let generation = beginItemOperation(.reloading)
        installItem()
        resume(at: mediaSeconds, generation: generation)
    }

    private func startRecovery() {
        reprepare(.recovering, at: timeline.recoveryStartSeconds(forMediaSeconds: positionSeconds))
    }

    private func reprepare(_ newOperation: ItemOperation, at mediaSeconds: Double) {
        guard !stopped else { return }
        let generation = beginItemOperation(newOperation)
        operationTask = Task { @MainActor [weak self] in
            await self?.performReprepare(at: mediaSeconds, generation: generation)
        }
    }

    private func performReprepare(at mediaSeconds: Double, generation: Int) async {
        guard generation == operationGeneration, !stopped else { return }
        let api = api
        let previousPlaybackID = playback.id
        log("preparing stream again at \(Int(mediaSeconds))s (\(operation.map { "\($0)" } ?? "idle"))")
        do {
            let prepared = try await api.prepareNativePlaybackWithRetry(
                playback,
                for: movie,
                startSeconds: mediaSeconds,
                audioStreamIndex: desiredAudioIndex,
                bitmapSubtitleIndex: desiredBitmapIndex
            )
            guard !Task.isCancelled, generation == operationGeneration, !stopped else {
                if prepared.playback.id != previousPlaybackID {
                    Task { try? await api.stopNativePlayback(prepared.playback.id) }
                }
                return
            }
            if prepared.playback.id != previousPlaybackID {
                Task { try? await api.stopNativePlayback(previousPlaybackID) }
            }
            apply(prepared)
            positionSeconds = mediaSeconds
            installItem()
            resume(at: mediaSeconds, generation: generation)
        } catch {
            guard !Task.isCancelled, generation == operationGeneration, !stopped else { return }
            log("preparing stream failed: \(error.localizedDescription)")
            operationFailed(error.localizedDescription)
        }
    }

    private func resume(at mediaSeconds: Double, generation: Int) {
        let playerSeconds = max(0, timeline.playerSeconds(forMediaSeconds: mediaSeconds))
        player.seek(
            to: CMTime(seconds: playerSeconds, preferredTimescale: 600),
            toleranceBefore: .zero,
            toleranceAfter: .zero
        ) { [weak self] finished in
            Task { @MainActor in
                self?.finishOperation(generation: generation, finished: finished)
            }
        }
    }

    private func finishOperation(generation: Int, finished: Bool) {
        guard generation == operationGeneration, !stopped else { return }
        if !finished {
            // An unfinished seek on a live item means the item failed; the watchdog
            // replaces it rather than ending playback here.
            log("seek did not finish")
        }
        operation = nil
        operationTask = nil
        pendingSeekSeconds = nil
        liveness.restart(now: Self.now, playerSeconds: currentPlayerSeconds)
        if wantsToPlay {
            player.play()
            startWatchdog()
        }
        refreshWaitingState()
        publishNowPlayingInfo()
    }

    private func operationFailed(_ message: String) {
        // Show what is actually packaged, so Retry does not repeat a failing choice.
        selectedAudio = audioOptions.first { $0.index == packagedAudioStreamIndex }
        if let selectedSubtitle, selectedSubtitle.isBitmap, selectedSubtitle.index != burnedSubtitleIndex {
            self.selectedSubtitle = burnedSubtitleIndex.flatMap { burned in
                subtitleOptions.first { $0.index == burned }
            }
        }
        fail(with: message)
    }

    private func fail(with message: String) {
        cancelOperation()
        stopWatchdog()
        wantsToPlay = false
        isPlaying = false
        isWaiting = false
        waitingDetail = nil
        stateLabel = "Unable to Play"
        errorMessage = message
        player.pause()
        publishNowPlayingInfo()
    }

    private func apply(_ prepared: PreparedPlayback) {
        let hls = prepared.hls
        playback = prepared.playback
        timeline = hls.timeline
        streamURL = hls.streamURL
        usesNativeSubtitles = hls.masterURL != nil
        burnedSubtitleIndex = hls.burnedSubtitleIndex
        packagedAudioStreamIndex = hls.audioStreamIndex
        audioOptions = hls.audioTracks ?? []
        selectedAudio = audioOptions.first { $0.index == hls.audioStreamIndex }
        let previous = selectedSubtitle
        subtitleOptions = hls.subtitles ?? []
        if let burned = hls.burnedSubtitleIndex {
            selectedSubtitle = subtitleOptions.first { $0.index == burned }
        } else if let previous, !previous.isBitmap {
            selectedSubtitle = subtitleOptions.first { $0.index == previous.index }
                ?? subtitleOptions.first {
                    !$0.isBitmap && $0.language == previous.language && $0.title == previous.title
                }
        } else {
            selectedSubtitle = nil
        }
        if let duration = hls.durationSeconds, duration > 0 {
            durationSeconds = duration
        }
    }

    // MARK: - Item installation and observation

    private func installItem() {
        removeItemObservers()
        itemGeneration += 1
        let generation = itemGeneration
        didReachEnd = false
        legibleGroup = nil
        subtitleTask?.cancel()
        subtitleTask = nil
        subtitleCues = []
        activeSubtitleText = nil

        let item = AVPlayerItem(url: cacheBusted(streamURL))
        item.preferredForwardBufferDuration = Self.preferredForwardBufferDuration
        itemObservations = [
            item.observe(\.status, options: [.new]) { [weak self] item, _ in
                let status = item.status
                let error = item.error
                Task { @MainActor in
                    self?.itemStatusChanged(status, error: error, generation: generation)
                }
            },
        ]
        let center = NotificationCenter.default
        itemObservers = [
            center.addObserver(forName: AVPlayerItem.didPlayToEndTimeNotification, object: item, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated {
                    self?.itemDidPlayToEnd(generation: generation)
                }
            },
            center.addObserver(forName: AVPlayerItem.failedToPlayToEndTimeNotification, object: item, queue: .main) { [weak self] notification in
                let error = notification.userInfo?[AVPlayerItemFailedToPlayToEndTimeErrorKey] as? Error
                MainActor.assumeIsolated {
                    self?.itemFailedToPlayToEnd(error, generation: generation)
                }
            },
            center.addObserver(forName: AVPlayerItem.playbackStalledNotification, object: item, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated {
                    self?.itemStalled(generation: generation)
                }
            },
            center.addObserver(forName: AVPlayerItem.newErrorLogEntryNotification, object: item, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated {
                    self?.logErrorLogEntry(generation: generation)
                }
            },
            center.addObserver(forName: AVPlayerItem.newAccessLogEntryNotification, object: item, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated {
                    self?.logAccessLogEntry(generation: generation)
                }
            },
        ]
        player.replaceCurrentItem(with: item)
        if usesNativeSubtitles {
            loadLegibleGroup(of: item, generation: generation)
        } else {
            restartSubtitleUpdates()
        }
    }

    private func removeItemObservers() {
        itemObservations.forEach { $0.invalidate() }
        itemObservations = []
        itemObservers.forEach { NotificationCenter.default.removeObserver($0) }
        itemObservers = []
    }

    private func itemStatusChanged(_ status: AVPlayerItem.Status, error: Error?, generation: Int) {
        guard !stopped, generation == itemGeneration else { return }
        switch status {
        case .readyToPlay:
            log("item ready")
        case .failed:
            log("item failed: \(error?.localizedDescription ?? "unknown error")")
            evaluateLiveness()
        default:
            break
        }
    }

    private func itemDidPlayToEnd(generation: Int) {
        guard !stopped, generation == itemGeneration else { return }
        cancelOperation()
        stopWatchdog()
        positionSeconds = max(positionSeconds, durationSeconds)
        wantsToPlay = false
        isPlaying = false
        isWaiting = false
        waitingDetail = nil
        didReachEnd = true
        stateLabel = "Episode Finished"
        publishNowPlayingInfo()
    }

    private func itemFailedToPlayToEnd(_ error: Error?, generation: Int) {
        guard !stopped, generation == itemGeneration else { return }
        log("item failed to play to end: \(error?.localizedDescription ?? "unknown error")")
        evaluateLiveness()
    }

    private func itemStalled(generation: Int) {
        guard !stopped, generation == itemGeneration else { return }
        log("playback stalled at \(Int(currentPlayerSeconds))s")
        refreshWaitingState()
    }

    private func logErrorLogEntry(generation: Int) {
        guard generation == itemGeneration,
              let event = player.currentItem?.errorLog()?.events.last else { return }
        log(
            "AVPlayer error \(event.errorStatusCode) \(event.errorDomain): "
                + "\(event.errorComment ?? "") \(event.uri ?? "")"
        )
    }

    private func logAccessLogEntry(generation: Int) {
        guard generation == itemGeneration,
              let event = player.currentItem?.accessLog()?.events.last else { return }
        log(
            "AVPlayer access: stalls=\(event.numberOfStalls) "
                + "observedBitrate=\(Int(event.observedBitrate)) startup=\(event.startupTime)"
        )
    }

    private func playerTimeAdvanced() {
        guard !stopped, operation == nil else { return }
        let current = currentPlayerSeconds
        guard current.isFinite, current >= 0 else { return }
        positionSeconds = min(
            timeline.mediaSeconds(forPlayerSeconds: current),
            durationSeconds > 0 ? durationSeconds : .greatestFiniteMagnitude
        )
        if !usesNativeSubtitles {
            updateActiveSubtitle()
        }
        if isWaiting {
            refreshWaitingState()
        }
        let elapsedSecond = Int(positionSeconds)
        if elapsedSecond / 5 != lastNowPlayingSecond / 5 {
            lastNowPlayingSecond = elapsedSecond
            publishNowPlayingInfo()
        }
    }

    private func timeControlStatusChanged(_ status: AVPlayer.TimeControlStatus, waitingReason: String?) {
        guard !stopped, errorMessage == nil, operation == nil else { return }
        switch status {
        case .playing, .waitingToPlayAtSpecifiedRate:
            if status == .waitingToPlayAtSpecifiedRate, let waitingReason {
                log("waiting: \(waitingReason)")
            }
            if !wantsToPlay {
                // Started outside the app's controls, for example Picture in Picture.
                wantsToPlay = true
                startWatchdog()
            }
        case .paused:
            if wantsToPlay, liveness.hasRenderedPlayback, !didReachEnd,
               let item = player.currentItem, item.status != .failed, item.error == nil {
                // Paused outside the app's controls: Picture in Picture or an audio
                // interruption. A stall waits instead of pausing.
                wantsToPlay = false
                stopWatchdog()
            }
        @unknown default:
            break
        }
        isPlaying = wantsToPlay
        refreshWaitingState()
        publishNowPlayingInfo()
    }

    private func refreshWaitingState() {
        guard operation == nil, errorMessage == nil else { return }
        let rendering = wantsToPlay && player.timeControlStatus == .playing
            && videoLayer?.isReadyForDisplay == true
        let waiting = wantsToPlay && !rendering
        if isWaiting != waiting {
            isWaiting = waiting
        }
        let label = rendering ? "Playing" : waiting ? "Buffering…" : didReachEnd ? "Episode Finished" : "Paused"
        if stateLabel != label {
            stateLabel = label
        }
        if !waiting, waitingDetail != nil {
            waitingDetail = nil
        }
    }

    private var currentPlayerSeconds: Double {
        player.currentTime().seconds
    }

    private func cacheBusted(_ url: URL) -> URL {
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: false) else {
            return url
        }
        var items = components.queryItems ?? []
        items.append(URLQueryItem(name: "seek", value: UUID().uuidString))
        components.queryItems = items
        return components.url ?? url
    }

    // MARK: - Liveness

    private func startWatchdog() {
        guard watchdogTask == nil, wantsToPlay, !stopped, errorMessage == nil else { return }
        liveness.restart(now: Self.now, playerSeconds: currentPlayerSeconds)
        watchdogTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: .seconds(1))
                } catch {
                    return
                }
                self?.evaluateLiveness()
            }
        }
        statusTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: .seconds(2))
                } catch {
                    return
                }
                guard let self else { return }
                if self.isWaiting {
                    await self.pollServerStatus()
                }
            }
        }
    }

    private func stopWatchdog() {
        watchdogTask?.cancel()
        watchdogTask = nil
        statusTask?.cancel()
        statusTask = nil
    }

    private func evaluateLiveness() {
        guard !stopped, wantsToPlay, errorMessage == nil, watchdogTask != nil else { return }
        let item = player.currentItem
        let sample = PlaybackLiveness.PlayerSample(
            playerSeconds: currentPlayerSeconds,
            isPlaying: player.timeControlStatus == .playing,
            isReadyForDisplay: videoLayer?.isReadyForDisplay == true,
            isLikelyToKeepUp: item?.isPlaybackLikelyToKeepUp == true,
            itemFailed: item == nil || item?.status == .failed
        )
        refreshWaitingState()
        switch liveness.observe(now: Self.now, player: sample, operationInFlight: operation != nil) {
        case .none:
            break
        case .reloadItem:
            log("reloading stalled item at \(Int(positionSeconds))s")
            reloadItem()
        case .reprepare:
            log("server stream needs preparing again")
            startRecovery()
        case .fail:
            log("giving up after stall")
            failStalledPlayback()
        }
    }

    private func pollServerStatus() async {
        let generation = operationGeneration
        let item = itemGeneration
        let playbackID = playback.id
        let sample: PlaybackLiveness.ServerSample
        do {
            sample = .status(try await api.playbackStatus(playbackID))
        } catch let FilmstreamError.server(status, _) where status == 404 {
            sample = .missing
        } catch {
            // An unreachable server is not evidence either way.
            return
        }
        guard !stopped, generation == operationGeneration, item == itemGeneration,
              playbackID == playback.id else { return }
        liveness.observeServer(sample, now: Self.now, playerSeconds: currentPlayerSeconds)
        guard case let .status(status) = sample else { return }
        if let error = status.hls?.error, !error.isEmpty {
            lastServerError = error
        }
        guard isWaiting else { return }
        let description = PlaybackProgressDescription(
            stage: .bufferingVideo(playbackID: playbackID),
            status: status
        )
        waitingDetail = [description.headline, description.detail].compactMap { $0 }.joined(separator: " · ")
    }

    private func failStalledPlayback() {
        var message = liveness.hasRenderedPlayback
            ? "Playback stalled: no new video arrived for over a minute."
            : "The video did not start: no data arrived for over a minute."
        if let waitingDetail {
            message += " Last status: \(waitingDetail)."
        } else if let lastServerError {
            message += " Server: \(lastServerError)."
        }
        fail(with: message)
    }

    // MARK: - Subtitles

    private func loadLegibleGroup(of item: AVPlayerItem, generation: Int) {
        let asset = item.asset
        subtitleTask = Task { @MainActor [weak self] in
            let group: AVMediaSelectionGroup?
            do {
                group = try await asset.loadMediaSelectionGroup(for: .legible)
            } catch {
                self?.log("subtitle renditions unavailable: \(error.localizedDescription)")
                return
            }
            guard let self, !Task.isCancelled, generation == self.itemGeneration else { return }
            self.legibleGroup = group
            self.applyNativeSubtitleSelection()
        }
    }

    private func applyNativeSubtitleSelection() {
        guard usesNativeSubtitles, let item = player.currentItem, let group = legibleGroup else { return }
        let option = selectedSubtitle.flatMap { track in
            track.isBitmap ? nil : renditionOption(for: track, in: group)
        }
        item.select(option, in: group)
    }

    private func renditionOption(
        for track: HLSSubtitleTrack,
        in group: AVMediaSelectionGroup
    ) -> AVMediaSelectionOption? {
        if let name = track.renditionName,
           let option = group.options.first(where: { $0.displayName == name }) {
            return option
        }
        // Renditions are listed in the same order as the text tracks.
        let textTracks = subtitleOptions.filter { !$0.isBitmap }
        guard group.options.count == textTracks.count,
              let position = textTracks.firstIndex(where: { $0.index == track.index }) else {
            return nil
        }
        return group.options[position]
    }

    /// Overlay fallback for servers without a master playlist: polls the growing
    /// WebVTT file of the selected text track.
    private func restartSubtitleUpdates() {
        subtitleTask?.cancel()
        subtitleTask = nil
        subtitleCues = []
        activeSubtitleText = nil
        guard !usesNativeSubtitles, let track = selectedSubtitle, !track.isBitmap, !stopped else { return }

        let api = api
        let playbackID = playback.id
        let generation = itemGeneration
        subtitleTask = Task { @MainActor [weak self] in
            do {
                try await api.startSubtitle(playbackID: playbackID, track: track)
            } catch {
                return
            }
            while !Task.isCancelled {
                guard let self, !self.stopped, generation == self.itemGeneration,
                      self.selectedSubtitle?.index == track.index else {
                    return
                }
                if let cues = try? await api.subtitleCues(playbackID: playbackID, track: track) {
                    guard !Task.isCancelled, generation == self.itemGeneration,
                          self.selectedSubtitle?.index == track.index else {
                        return
                    }
                    self.subtitleCues = cues
                    self.updateActiveSubtitle()
                }
                do {
                    try await Task.sleep(for: .seconds(2))
                } catch {
                    return
                }
            }
        }
    }

    private func updateActiveSubtitle() {
        let text = subtitleCues
            .filter { positionSeconds >= $0.startSeconds && positionSeconds <= $0.endSeconds }
            .map(\.text)
            .joined(separator: "\n")
        let active = text.isEmpty ? nil : text
        if activeSubtitleText != active {
            activeSubtitleText = active
        }
    }

    // MARK: - Diagnostics

    private func log(_ message: String) {
        let playbackID = playback.id
        Self.logger.info("[\(playbackID, privacy: .public)] \(message, privacy: .public)")
    }

    // MARK: - System media session

    /// Registers this player with the system's Now Playing and remote commands so
    /// headphone and remote play/pause controls operate it.
    public func activateMediaSession() {
        #if os(iOS) || os(tvOS)
        guard mediaSessionID == nil, !stopped else { return }
        let audioSession = AVAudioSession.sharedInstance()
        try? audioSession.setCategory(.playback, mode: .moviePlayback)
        try? audioSession.setActive(true)

        let commands = MPRemoteCommandCenter.shared()
        commands.playCommand.isEnabled = true
        commands.pauseCommand.isEnabled = true
        commands.togglePlayPauseCommand.isEnabled = true
        let playTarget = commands.playCommand.addTarget { [weak self] _ in
            Task { @MainActor in self?.play() }
            return .success
        }
        let pauseTarget = commands.pauseCommand.addTarget { [weak self] _ in
            Task { @MainActor in self?.pause() }
            return .success
        }
        let toggleTarget = commands.togglePlayPauseCommand.addTarget { [weak self] _ in
            Task { @MainActor in self?.togglePlayback() }
            return .success
        }
        remoteCommandTargets = [
            (commands.playCommand, playTarget),
            (commands.pauseCommand, pauseTarget),
            (commands.togglePlayPauseCommand, toggleTarget),
        ]
        let sessionID = UUID().uuidString
        mediaSessionID = sessionID
        Self.activeMediaSessionID = sessionID
        publishNowPlayingInfo()
        #endif
    }

    private func publishNowPlayingInfo() {
        #if os(iOS) || os(tvOS)
        guard mediaSessionID != nil else { return }
        var info: [String: Any] = [
            MPMediaItemPropertyTitle: movie.episodeTitle ?? movie.title,
            MPNowPlayingInfoPropertyExternalContentIdentifier: mediaSessionID ?? playback.id,
            MPNowPlayingInfoPropertyElapsedPlaybackTime: positionSeconds,
            MPNowPlayingInfoPropertyPlaybackRate: wantsToPlay ? 1.0 : 0.0,
        ]
        if let seriesTitle = movie.seriesTitle {
            info[MPMediaItemPropertyAlbumTitle] = seriesTitle
        }
        if let episodeLabel = movie.episodeLabel {
            info[MPMediaItemPropertyArtist] = episodeLabel
        }
        if durationSeconds > 0 {
            info[MPMediaItemPropertyPlaybackDuration] = durationSeconds
        }
        let center = MPNowPlayingInfoCenter.default()
        center.nowPlayingInfo = info
        center.playbackState = wantsToPlay ? .playing : .paused
        #endif
    }

    private func deactivateMediaSession() {
        #if os(iOS) || os(tvOS)
        guard let sessionID = mediaSessionID else { return }
        for (command, target) in remoteCommandTargets {
            command.removeTarget(target)
        }
        remoteCommandTargets.removeAll()
        mediaSessionID = nil
        guard Self.activeMediaSessionID == sessionID else { return }
        Self.activeMediaSessionID = nil
        let commands = MPRemoteCommandCenter.shared()
        commands.playCommand.isEnabled = false
        commands.pauseCommand.isEnabled = false
        commands.togglePlayPauseCommand.isEnabled = false
        let center = MPNowPlayingInfoCenter.default()
        center.nowPlayingInfo = nil
        center.playbackState = .stopped
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        #endif
    }
}
