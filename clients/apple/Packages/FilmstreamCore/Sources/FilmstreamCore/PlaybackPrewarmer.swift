import Foundation

public actor PlaybackPrewarmer {
    private struct Target: Sendable {
        let movie: Movie
        let startSeconds: Double
    }

    private struct WarmedPlayback: Sendable {
        let target: Target
        let prepared: PreparedPlayback
    }

    private struct InFlight: Sendable {
        let token: UUID
        let target: Target
        let task: Task<PreparedPlayback?, Never>
    }

    private var desired: [String: Target] = [:]
    private var warmed: [String: WarmedPlayback] = [:]
    private var inFlight: [String: InFlight] = [:]
    private var suppressed: [String: Double] = [:]
    private var worker: Task<Void, Never>?

    public init() {}

    public func synchronize(
        with entries: [WatchHistoryEntry],
        using api: FilmstreamAPI
    ) {
        let targets = entries.compactMap { entry -> Target? in
            guard !entry.completed, entry.positionSeconds > 0 else { return nil }
            return Target(movie: entry.playbackMovie, startSeconds: entry.positionSeconds)
        }
        desired = Dictionary(targets.map { ($0.movie.id, $0) }, uniquingKeysWith: { first, _ in first })

        for (mediaID, position) in suppressed {
            guard let target = desired[mediaID], matches(position, target.startSeconds) else {
                suppressed.removeValue(forKey: mediaID)
                continue
            }
        }

        for (mediaID, cached) in warmed {
            guard let target = desired[mediaID], matches(cached.target, target) else {
                warmed.removeValue(forKey: mediaID)
                Task { try? await api.stopNativePlayback(cached.prepared.playback.id) }
                continue
            }
        }

        for (mediaID, flight) in inFlight {
            guard let target = desired[mediaID], matches(flight.target, target) else {
                inFlight.removeValue(forKey: mediaID)
                flight.task.cancel()
                continue
            }
        }

        worker?.cancel()
        worker = Task { [weak self] in
            for target in targets {
                guard !Task.isCancelled else { return }
                _ = await self?.warm(target, using: api)
            }
        }
    }

    public func preparedPlayback(
        for movie: Movie,
        startSeconds: Double,
        using api: FilmstreamAPI
    ) async -> PreparedPlayback? {
        let requested = Target(movie: movie, startSeconds: startSeconds)
        let hasMatchingWork = warmed[movie.id].map { matches($0.target, requested) } == true
            || inFlight[movie.id].map { matches($0.target, requested) } == true
            || desired[movie.id].map { matches($0, requested) } == true
        guard hasMatchingWork else {
            if let stale = warmed.removeValue(forKey: movie.id) {
                Task { try? await api.stopNativePlayback(stale.prepared.playback.id) }
            }
            suppressed[movie.id] = startSeconds
            return nil
        }

        guard let cached = await warm(requested, using: api) else {
            suppressed[movie.id] = startSeconds
            return nil
        }
        warmed.removeValue(forKey: movie.id)
        suppressed[movie.id] = startSeconds

        do {
            let resumed = try await api.prepareNativePlayback(
                cached.playback,
                startSeconds: startSeconds
            )
            return PreparedPlayback(playback: cached.playback, hls: resumed.hls)
        } catch {
            try? await api.stopNativePlayback(cached.playback.id)
            return nil
        }
    }

    public func remove(_ entry: WatchHistoryEntry, using api: FilmstreamAPI) {
        let mediaID = entry.playbackMovie.id
        desired.removeValue(forKey: mediaID)
        suppressed.removeValue(forKey: mediaID)
        if let flight = inFlight.removeValue(forKey: mediaID) {
            flight.task.cancel()
        }
        if let cached = warmed.removeValue(forKey: mediaID) {
            Task { try? await api.stopNativePlayback(cached.prepared.playback.id) }
        }
    }

    private func warm(_ target: Target, using api: FilmstreamAPI) async -> PreparedPlayback? {
        let mediaID = target.movie.id
        if let position = suppressed[mediaID], matches(position, target.startSeconds) {
            return nil
        }
        if let cached = warmed[mediaID], matches(cached.target, target) {
            return cached.prepared
        }
        if let cached = warmed.removeValue(forKey: mediaID) {
            Task { try? await api.stopNativePlayback(cached.prepared.playback.id) }
        }

        let flight: InFlight
        if let existing = inFlight[mediaID], matches(existing.target, target) {
            flight = existing
        } else {
            if let existing = inFlight.removeValue(forKey: mediaID) {
                existing.task.cancel()
            }
            let task = Task<PreparedPlayback?, Never> {
                var playback: Playback?
                do {
                    let created = try await api.createPlayback(for: target.movie)
                    playback = created
                    let prepared = try await api.prepareNativePlayback(
                        created,
                        startSeconds: target.startSeconds
                    )
                    try await api.parkNativePlayback(created.id)
                    return prepared
                } catch {
                    if let playback {
                        try? await api.stopNativePlayback(playback.id)
                    }
                    return nil
                }
            }
            flight = InFlight(token: UUID(), target: target, task: task)
            inFlight[mediaID] = flight
        }

        let prepared = await flight.task.value
        if inFlight[mediaID]?.token == flight.token {
            inFlight.removeValue(forKey: mediaID)
        }
        guard let prepared else { return nil }
        guard let current = desired[mediaID], matches(current, target),
              suppressed[mediaID].map({ matches($0, target.startSeconds) }) != true else {
            try? await api.stopNativePlayback(prepared.playback.id)
            return nil
        }
        if let existing = warmed[mediaID] {
            if existing.prepared.playback.id != prepared.playback.id {
                try? await api.stopNativePlayback(prepared.playback.id)
            }
            return existing.prepared
        }
        warmed[mediaID] = WarmedPlayback(target: target, prepared: prepared)
        return prepared
    }

    private func matches(_ left: Target, _ right: Target) -> Bool {
        left.movie.id == right.movie.id && matches(left.startSeconds, right.startSeconds)
    }

    private func matches(_ left: Double, _ right: Double) -> Bool {
        abs(left - right) <= 0.5
    }
}
