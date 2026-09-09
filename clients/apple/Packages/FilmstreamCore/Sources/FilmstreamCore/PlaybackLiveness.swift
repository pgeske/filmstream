import Foundation

/// Readiness is evidence from the renderer and advancing player time, not an HLS
/// response, AVPlayerItem.readyToPlay, or timeControlStatus alone.
public struct PlaybackLiveness: Sendable {
    public enum Action: Equatable, Sendable {
        case none
        case recover
        case fail
    }

    private var lastProgressAt: TimeInterval
    private var lastPlayerSeconds: Double
    private var attemptedRecovery = false
    private var failed = false
    public private(set) var hasRenderedPlayback = false

    public init(now: TimeInterval, playerSeconds: Double) {
        lastProgressAt = now
        lastPlayerSeconds = playerSeconds
    }

    public mutating func observe(
        now: TimeInterval,
        playerSeconds: Double,
        isPlaying: Bool,
        isReadyForDisplay: Bool,
        canRecover: Bool
    ) -> Action {
        guard !failed else { return .none }
        let advanced = playerSeconds.isFinite && lastPlayerSeconds.isFinite
            && playerSeconds > lastPlayerSeconds
        lastPlayerSeconds = playerSeconds
        if isPlaying, isReadyForDisplay, advanced {
            hasRenderedPlayback = true
            lastProgressAt = now
            attemptedRecovery = false
            return .none
        }

        let elapsed = now - lastProgressAt
        if elapsed >= TimeInterval(NativePlaybackConfiguration.stallTimeout.components.seconds) {
            failed = true
            return .fail
        }
        if canRecover, !attemptedRecovery,
           elapsed >= TimeInterval(NativePlaybackConfiguration.recoveryDelay.components.seconds) {
            attemptedRecovery = true
            return .recover
        }
        return .none
    }
}
