import Foundation
import Observation

/// Owns one presentation request. Cancellation revokes presentation authority even
/// if an underlying request completes successfully after the screen has gone away.
@MainActor
@Observable
public final class PlaybackPreparation {
    public private(set) var stage: PlaybackPreparationStage?
    /// Latest server status of the playback being prepared.
    public private(set) var status: PlaybackStatus?
    public private(set) var errorMessage: String?
    public var isPreparing: Bool { stage != nil }
    public var progress: PlaybackProgressDescription? {
        stage.map { PlaybackProgressDescription(stage: $0, status: status) }
    }
    public var canRetry: Bool { errorMessage != nil && retryRequest != nil }

    private var generation = 0
    private var task: Task<Void, Never>?
    private var statusTask: Task<Void, Never>?
    private var fetchStatus: @MainActor (String) async -> PlaybackStatus? = { _ in nil }
    private var retryRequest: (@MainActor () -> Void)?

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
                try await api.preparePlayback(for: movie, startSeconds: startSeconds, onStage: onStage)
            },
            status: { try? await api.playbackStatus($0) },
            nextEpisode: nextEpisode,
            discard: { try? await api.stopNativePlayback($0.playback.id) },
            onPrepared: onPrepared
        )
        retryRequest = { [weak self] in
            self?.start(
                api: api,
                movie: movie,
                startSeconds: startSeconds,
                nextEpisode: nextEpisode,
                onPrepared: onPrepared
            )
        }
    }

    /// Repeats the last request after it failed.
    public func retry() {
        retryRequest?()
    }

    // Kept internal so tests can hold completions past cancellation without a server.
    func start(
        prepare: @escaping @MainActor (
            @escaping @MainActor @Sendable (PlaybackPreparationStage) -> Void
        ) async throws -> PreparedPlayback,
        status: @escaping @MainActor (String) async -> PlaybackStatus? = { _ in nil },
        nextEpisode: @escaping @MainActor () async -> Episode? = { nil },
        discard: @escaping @MainActor (PreparedPlayback) async -> Void,
        onPrepared: @escaping @MainActor (PreparedPlayback, Episode?) -> Void
    ) {
        cancel()
        let requestGeneration = generation
        fetchStatus = status
        stage = .findingRelease
        task = Task { @MainActor in
            do {
                async let next = nextEpisode()
                let prepared = try await prepare { stage in
                    guard self.generation == requestGeneration else { return }
                    self.stage = stage
                    if case let .bufferingVideo(playbackID) = stage {
                        self.pollStatus(of: playbackID, generation: requestGeneration)
                    }
                }
                let episode = await next
                guard !Task.isCancelled, self.generation == requestGeneration else {
                    // Cleanup must not inherit the canceled request's Task flag.
                    await Task { await discard(prepared) }.value
                    return
                }
                self.finish()
                onPrepared(prepared, episode)
            } catch {
                guard !Task.isCancelled, self.generation == requestGeneration else { return }
                self.finish()
                self.errorMessage = error.localizedDescription
            }
        }
    }

    /// Abandons the current request and clears any error it reported.
    public func cancel() {
        generation += 1
        task?.cancel()
        finish()
        errorMessage = nil
    }

    private func finish() {
        task = nil
        statusTask?.cancel()
        statusTask = nil
        stage = nil
        status = nil
    }

    private func pollStatus(of playbackID: String, generation requestGeneration: Int) {
        statusTask?.cancel()
        statusTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                guard let fetch = self?.fetchStatus else { return }
                let status = await fetch(playbackID)
                guard let self, !Task.isCancelled, self.generation == requestGeneration else { return }
                if let status {
                    self.status = status
                }
                do {
                    try await Task.sleep(for: .seconds(1))
                } catch {
                    return
                }
            }
        }
    }
}
