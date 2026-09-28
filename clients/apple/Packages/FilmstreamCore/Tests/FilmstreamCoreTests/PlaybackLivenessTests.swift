import Testing
@testable import FilmstreamCore

private typealias Sample = PlaybackLiveness.PlayerSample

private let starting = Sample(playerSeconds: 0, isPlaying: false, isReadyForDisplay: false)

@Test func serverProgressKeepsAStartingPlayerWaiting() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    // A cold swarm: no video for 100 s, but torrent data keeps arriving.
    monitor.observeServer(
        .status(PlaybackStatus(activePeers: 3, downloadRate: 500_000, hls: .init(state: "starting", packagedSeconds: 2))),
        now: 80,
        playerSeconds: 0
    )
    #expect(monitor.observe(now: 100, player: starting, operationInFlight: false) == .none)

    // The swarm goes silent: the budget runs from the last server progress.
    monitor.observeServer(
        .status(PlaybackStatus(activePeers: 0, downloadRate: 0, hls: .init(state: "starting", packagedSeconds: 2))),
        now: 150,
        playerSeconds: 0
    )
    #expect(monitor.observe(now: 169, player: starting, operationInFlight: false) == .none)
    #expect(monitor.observe(now: 170, player: starting, operationInFlight: false) == .fail)
    #expect(!monitor.hasRenderedPlayback)
    // Late evidence cannot revive terminal failure.
    let playing = Sample(playerSeconds: 1, isPlaying: true, isReadyForDisplay: true)
    #expect(monitor.observe(now: 171, player: playing, operationInFlight: false) == .none)
    #expect(!monitor.hasRenderedPlayback)
}

@Test func inFlightOperationHoldsTheDeadlineAndInstallationRestartsIt() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    #expect(monitor.observe(now: 89, player: starting, operationInFlight: false) == .none)
    // A re-prepare that waits a minute for the swarm is not a stall.
    #expect(monitor.observe(now: 150, player: starting, operationInFlight: true) == .none)
    monitor.restart(now: 150, playerSeconds: 0)
    #expect(monitor.observe(now: 239, player: starting, operationInFlight: false) == .none)
    #expect(monitor.observe(now: 240, player: starting, operationInFlight: false) == .fail)
}

@Test func failedItemIsReloadedBeforeTheServerPreparesItAgain() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    let failed = Sample(playerSeconds: 0, isPlaying: false, isReadyForDisplay: false, itemFailed: true)
    #expect(monitor.observe(now: 1, player: failed, operationInFlight: false) == .reloadItem)
    monitor.restart(now: 2, playerSeconds: 0)
    #expect(monitor.observe(now: 3, player: failed, operationInFlight: false) == .reprepare)
    monitor.restart(now: 4, playerSeconds: 0)
    #expect(monitor.observe(now: 5, player: failed, operationInFlight: false) == .fail)
}

@Test func serverReportedFailureSkipsTheLocalReload() {
    var failedStream = PlaybackLiveness(now: 0, playerSeconds: 0)
    failedStream.observeServer(
        .status(PlaybackStatus(hls: .init(state: "failed", error: "ffmpeg exited"))),
        now: 1,
        playerSeconds: 0
    )
    #expect(failedStream.observe(now: 1, player: starting, operationInFlight: false) == .reprepare)

    var lostPlayback = PlaybackLiveness(now: 0, playerSeconds: 0)
    lostPlayback.observeServer(.missing, now: 1, playerSeconds: 0)
    #expect(lostPlayback.observe(now: 1, player: starting, operationInFlight: false) == .reprepare)
}

@Test func advancingVideoEndsTheStallAndRenewsItsRemedies() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    let playing = Sample(playerSeconds: 5, isPlaying: true, isReadyForDisplay: true)
    #expect(monitor.observe(now: 1, player: playing, operationInFlight: false) == .none)
    #expect(monitor.hasRenderedPlayback)

    let failed = Sample(playerSeconds: 5, isPlaying: false, isReadyForDisplay: true, itemFailed: true)
    #expect(monitor.observe(now: 2, player: failed, operationInFlight: false) == .reloadItem)
    monitor.restart(now: 3, playerSeconds: 5)
    let resumed = Sample(playerSeconds: 6, isPlaying: true, isReadyForDisplay: true)
    #expect(monitor.observe(now: 4, player: resumed, operationInFlight: false) == .none)
    // A later, independent stall starts with the cheap remedy again.
    let failedAgain = Sample(playerSeconds: 6, isPlaying: false, isReadyForDisplay: true, itemFailed: true)
    #expect(monitor.observe(now: 5, player: failedAgain, operationInFlight: false) == .reloadItem)
}

@Test func playerHoldingDataWithoutPlayingEscalatesAndIsNotExcusedByDownloads() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    let wedged = Sample(playerSeconds: 30, isPlaying: false, isReadyForDisplay: true, isLikelyToKeepUp: true)
    #expect(monitor.observe(now: 9, player: wedged, operationInFlight: false) == .none)
    #expect(monitor.observe(now: 10, player: wedged, operationInFlight: false) == .reloadItem)
    monitor.restart(now: 11, playerSeconds: 30)
    #expect(monitor.observe(now: 20, player: wedged, operationInFlight: false) == .none)
    #expect(monitor.observe(now: 21, player: wedged, operationInFlight: false) == .reprepare)
    monitor.restart(now: 22, playerSeconds: 30)

    monitor.observeServer(.status(PlaybackStatus(activePeers: 5, downloadRate: 1_000_000)), now: 100, playerSeconds: 30)
    #expect(monitor.observe(now: 111, player: wedged, operationInFlight: false) == .none)
    #expect(monitor.observe(now: 112, player: wedged, operationInFlight: false) == .fail)
}

@Test func packagedMediaAheadOfThePlayerCountsAsAvailableData() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    monitor.observeServer(.status(PlaybackStatus(hls: .init(state: "ready", packagedSeconds: 7.9))), now: 5, playerSeconds: 0)
    #expect(monitor.observe(now: 10, player: starting, operationInFlight: false) == .none)
    monitor.observeServer(.status(PlaybackStatus(hls: .init(state: "ready", packagedSeconds: 12))), now: 11, playerSeconds: 0)
    #expect(monitor.observe(now: 11, player: starting, operationInFlight: false) == .reloadItem)
}

@Test func runningClockWithoutDisplayedVideoIsNotExcusedByDownloads() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    monitor.observeServer(.status(PlaybackStatus(downloadRate: 2_000_000)), now: 85, playerSeconds: 85)
    let blind = Sample(playerSeconds: 90, isPlaying: true, isReadyForDisplay: false)
    #expect(monitor.observe(now: 90, player: blind, operationInFlight: false) == .fail)
}
