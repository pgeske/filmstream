import Foundation
import Testing
@testable import FilmstreamCore

@Suite(.timeLimit(.minutes(1)))
@MainActor
struct PlaybackPreparationTests {
    @Test func dismissedPreparationCannotPresentOrClearNewRequest() async throws {
        let owner = PlaybackPreparation()
        let old = PreparationGate()
        let new = PreparationGate()
        let discarded = AsyncStream.makeStream(of: String.self)
        let presented = AsyncStream.makeStream(of: String.self)
        var discardEvents = discarded.stream.makeAsyncIterator()
        var presentationEvents = presented.stream.makeAsyncIterator()
        var presentations: [String] = []
        let onPrepared: @MainActor (PreparedPlayback, Episode?) -> Void = { value, _ in
            presentations.append(value.id)
            presented.continuation.yield(value.id)
        }

        owner.start(prepare: old.prepare, discard: { value in
            #expect(!Task.isCancelled)
            discarded.continuation.yield(value.id)
        }, onPrepared: onPrepared)
        await old.waitUntilStarted()
        owner.cancel() // Back before the HTTP response, then re-enter the same screen.
        owner.start(prepare: new.prepare, discard: { _ in }, onPrepared: onPrepared)
        await new.waitUntilStarted()

        old.onStage?(.bufferingVideo)
        old.complete(try prepared("old")) // A transport that ignores cancellation.
        #expect(await discardEvents.next() == "old")
        #expect(owner.isPreparing)
        #expect(owner.stage == .findingRelease)
        #expect(owner.errorMessage == nil)
        #expect(presentations.isEmpty)

        new.complete(try prepared("new"))
        #expect(await presentationEvents.next() == "new")
        #expect(!owner.isPreparing)
        #expect(presentations == ["new"])
    }

    @Test func cancellationAfterHLSWhileAwaitingNextEpisodeDoesNotReopenPlayer() async throws {
        let owner = PlaybackPreparation()
        let metadata = AsyncStream.makeStream(of: Void.self)
        let discarded = AsyncStream.makeStream(of: String.self)
        var metadataEvents = metadata.stream.makeAsyncIterator()
        var discardEvents = discarded.stream.makeAsyncIterator()
        var finishMetadata: CheckedContinuation<Episode?, Never>?
        let value = try prepared("prepared-before-dismissal")
        var presented = false
        owner.start(prepare: { _ in value }, nextEpisode: {
            await withCheckedContinuation {
                finishMetadata = $0
                metadata.continuation.yield(())
            }
        }, discard: { discarded.continuation.yield($0.id) }, onPrepared: { _, _ in presented = true })
        _ = await metadataEvents.next()
        owner.cancel()
        finishMetadata?.resume(returning: nil)
        #expect(await discardEvents.next() == value.id)
        #expect(!presented)
        #expect(!owner.isPreparing)
    }

    private func prepared(_ id: String) throws -> PreparedPlayback {
        let playback = try JSONDecoder().decode(Playback.self, from: Data("""
        {"id":"\(id)","name":"Episode","file_name":"episode.mkv","file_size":1000,"stream_url":"https://fixture.test/stream"}
        """.utf8))
        let hls = try JSONDecoder().decode(HLSPlayback.self, from: Data("""
        {"playback_id":"\(id)","playlist_url":"https://fixture.test/index.m3u8","start_seconds":0,"video_codec":"h264"}
        """.utf8))
        return PreparedPlayback(playback: playback, hls: hls)
    }
}

@MainActor
private final class PreparationGate {
    private let started = AsyncStream.makeStream(of: Void.self)
    private var completion: CheckedContinuation<PreparedPlayback, Never>?
    var onStage: (@MainActor (PlaybackPreparationStage) -> Void)?

    func prepare(onStage: @escaping @MainActor (PlaybackPreparationStage) -> Void) async -> PreparedPlayback {
        self.onStage = onStage
        return await withCheckedContinuation {
            completion = $0
            started.continuation.yield(())
        }
    }

    func waitUntilStarted() async {
        var events = started.stream.makeAsyncIterator()
        _ = await events.next()
    }

    func complete(_ prepared: PreparedPlayback) {
        completion?.resume(returning: prepared)
        completion = nil
    }
}
