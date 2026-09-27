import Foundation

/// Decides when a waiting player needs help.
///
/// Progress is evidence from the renderer (displayed video whose time advances) or
/// from the server (new packaged media or torrent data arriving). While the server
/// makes progress the player keeps waiting instead of failing. The first remedy is a
/// local item reload with the same URL; the server is only asked to prepare the
/// stream again when it reports that the stream failed, or when the reloaded item
/// still fails or holds data without playing it.
/// Each remedy is tried at most once per stall, and a stall ends only when video
/// actually advances.
public struct PlaybackLiveness: Sendable {
    public enum Action: Equatable, Sendable {
        case none
        /// Replace the player item with one for the same URL at the current position.
        case reloadItem
        /// Ask the server to prepare the stream again.
        case reprepare
        /// Give up: neither the player nor the server has progressed for `stallTimeout`,
        /// or every remedy has been tried.
        case fail
    }

    public struct PlayerSample: Equatable, Sendable {
        public var playerSeconds: Double
        /// `AVPlayer.timeControlStatus == .playing`.
        public var isPlaying: Bool
        public var isReadyForDisplay: Bool
        public var isLikelyToKeepUp: Bool
        public var itemFailed: Bool

        public init(
            playerSeconds: Double,
            isPlaying: Bool,
            isReadyForDisplay: Bool,
            isLikelyToKeepUp: Bool = false,
            itemFailed: Bool = false
        ) {
            self.playerSeconds = playerSeconds
            self.isPlaying = isPlaying
            self.isReadyForDisplay = isReadyForDisplay
            self.isLikelyToKeepUp = isLikelyToKeepUp
            self.itemFailed = itemFailed
        }
    }

    public enum ServerSample: Equatable, Sendable {
        case status(PlaybackStatus)
        /// The server no longer knows this playback.
        case missing
    }

    /// A player that has data but does not advance for this long gets a fresh item.
    public static let wedgedReloadDelay: TimeInterval = 10
    /// Without progress from the player or the server for this long, playback fails.
    public static let stallTimeout: TimeInterval = 90
    /// Packaged media at least this far ahead of the player means data is available.
    static let availableAheadSeconds: Double = 8
    /// A server-reported packaging advance within this window counts as progress.
    static let serverProgressWindow: TimeInterval = 10

    private var lastProgressAt: TimeInterval
    private var lastServerProgressAt: TimeInterval
    private var lastPlayerSeconds: Double
    private var lastPackagedSeconds: Double?
    private var serverHasDataAhead = false
    private var serverNeedsReprepare = false
    private var reloadedThisStall = false
    private var repreparedThisStall = false
    private var failed = false
    public private(set) var hasRenderedPlayback = false

    public init(now: TimeInterval, playerSeconds: Double) {
        lastProgressAt = now
        lastServerProgressAt = now
        lastPlayerSeconds = playerSeconds
    }

    /// A new item was installed (reload, re-prepare, or seek). Its timeline and server
    /// stream are new, so earlier observations no longer apply and the budget restarts.
    /// Remedies already tried in this stall stay used until video advances.
    public mutating func restart(now: TimeInterval, playerSeconds: Double) {
        lastProgressAt = now
        lastServerProgressAt = now
        lastPlayerSeconds = playerSeconds
        lastPackagedSeconds = nil
        serverHasDataAhead = false
        serverNeedsReprepare = false
    }

    public mutating func observeServer(_ sample: ServerSample, now: TimeInterval, playerSeconds: Double) {
        guard case let .status(status) = sample else {
            serverNeedsReprepare = true
            return
        }
        serverNeedsReprepare = status.needsReprepare
        let packaging = status.hls
        var progressed = (status.downloadRate ?? 0) > 0 || packaging?.throttled == true
        if let sinceProgress = packaging?.secondsSinceProgress, sinceProgress < Self.serverProgressWindow {
            progressed = true
        }
        if let packaged = packaging?.packagedSeconds {
            if let last = lastPackagedSeconds, packaged > last {
                progressed = true
            }
            lastPackagedSeconds = packaged
            let required = packaging?.complete == true ? 1 : Self.availableAheadSeconds
            serverHasDataAhead = playerSeconds.isFinite && packaged - playerSeconds >= required
        } else {
            serverHasDataAhead = false
        }
        if progressed {
            lastServerProgressAt = now
        }
    }

    /// `operationInFlight` is true while a seek, reload, or re-prepare owns the item:
    /// that wait is not a stall, so the deadline is held until `restart`.
    public mutating func observe(
        now: TimeInterval,
        player: PlayerSample,
        operationInFlight: Bool
    ) -> Action {
        guard !failed else { return .none }
        let advanced = player.playerSeconds.isFinite && lastPlayerSeconds.isFinite
            && player.playerSeconds > lastPlayerSeconds
        lastPlayerSeconds = player.playerSeconds
        if operationInFlight {
            lastProgressAt = now
            lastServerProgressAt = now
            return .none
        }
        if player.isPlaying, player.isReadyForDisplay, advanced {
            hasRenderedPlayback = true
            lastProgressAt = now
            reloadedThisStall = false
            repreparedThisStall = false
            return .none
        }

        if serverNeedsReprepare || player.itemFailed {
            if !serverNeedsReprepare, !reloadedThisStall {
                reloadedThisStall = true
                return .reloadItem
            }
            if !repreparedThisStall {
                repreparedThisStall = true
                return .reprepare
            }
            failed = true
            return .fail
        }

        // Data is available, yet video does not advance: waiting longer will not help.
        let wedged = player.isLikelyToKeepUp || serverHasDataAhead
        if wedged, now - lastProgressAt >= Self.wedgedReloadDelay {
            if !reloadedThisStall {
                reloadedThisStall = true
                return .reloadItem
            }
            if !repreparedThisStall {
                repreparedThisStall = true
                return .reprepare
            }
        }

        // A wedged player, or a clock that runs without displayed video, is not
        // starved of data, so server downloads do not excuse it.
        let clockRunsWithoutVideo = advanced && player.isPlaying && !player.isReadyForDisplay
        let lastEvidence = wedged || clockRunsWithoutVideo
            ? lastProgressAt
            : max(lastProgressAt, lastServerProgressAt)
        if now - lastEvidence >= Self.stallTimeout {
            failed = true
            return .fail
        }
        return .none
    }
}
