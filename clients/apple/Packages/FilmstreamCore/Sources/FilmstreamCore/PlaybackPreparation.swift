import Foundation
import Observation

/// Owns one presentation request. Cancellation revokes presentation authority even
/// if an underlying request completes successfully after the screen has gone away.
@MainActor
@Observable
public final class PlaybackPreparation {
    public private(set) var stage: PlaybackPreparationStage?
    public private(set) var errorMessage: String?
    public var isPreparing: Bool { stage != nil }

    private var generation = 0
    private var task: Task<Void, Never>?

    public init() {}

    public func start(
        api: FilmstreamAPI,
        movie: Movie,
        startSeconds: Double,
        nextEpisode: @escaping @MainActor () async -> Episode? = { nil },
        onPrepared: @escaping @MainActor (PreparedPlayback, Episode?) -> Void
    ) {
        start(
            prepare: { onStage in
                onStage(.findingRelease)
                let playback = try await api.createPlayback(for: movie, startSeconds: startSeconds)
                do {
                    try Task.checkCancellation()
                    onStage(.bufferingVideo)
                    return try await api.prepareNativePlaybackWithRetry(
                        playback, for: movie, startSeconds: startSeconds
                    )
                } catch {
                    // This request owns a newly created/claimed session, not the
                    // in-place session of a newer seek or recovery.
                    Task { try? await api.stopNativePlayback(playback.id) }
                    throw error
                }
            },
            nextEpisode: nextEpisode,
            discard: { try? await api.stopNativePlayback($0.playback.id) },
            onPrepared: onPrepared
        )
    }

    // Kept internal so tests can hold completions past cancellation without a server.
    func start(
        prepare: @escaping @MainActor (@escaping @MainActor (PlaybackPreparationStage) -> Void) async throws -> PreparedPlayback,
        nextEpisode: @escaping @MainActor () async -> Episode? = { nil },
        discard: @escaping @MainActor (PreparedPlayback) async -> Void,
        onPrepared: @escaping @MainActor (PreparedPlayback, Episode?) -> Void
    ) {
        cancel()
        let requestGeneration = generation
        stage = .findingRelease
        errorMessage = nil
        task = Task { @MainActor in
            do {
                async let next = nextEpisode()
                let prepared = try await prepare { stage in
                    guard self.generation == requestGeneration else { return }
                    self.stage = stage
                }
                let episode = await next
                guard !Task.isCancelled, self.generation == requestGeneration else {
                    // Cleanup must not inherit the canceled request's Task flag.
                    await Task { await discard(prepared) }.value
                    return
                }
                self.task = nil
                self.stage = nil
                onPrepared(prepared, episode)
            } catch {
                guard !Task.isCancelled, self.generation == requestGeneration else { return }
                self.task = nil
                self.stage = nil
                self.errorMessage = error.localizedDescription
            }
        }
    }

    public func cancel() {
        generation += 1
        task?.cancel()
        task = nil
        stage = nil
    }
}
