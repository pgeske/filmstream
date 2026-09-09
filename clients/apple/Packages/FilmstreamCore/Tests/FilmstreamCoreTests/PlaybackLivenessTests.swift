import Testing
@testable import FilmstreamCore

@Test func readyItemOrPlayingClockWithoutRenderedVideoDoesNotRenewStartupBudget() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    // Even a ready-to-play item with an advancing clock is not displayed video.
    #expect(monitor.observe(now: 11, playerSeconds: 11, isPlaying: true, isReadyForDisplay: false, canRecover: true) == .none)
    #expect(monitor.observe(now: 12, playerSeconds: 12, isPlaying: true, isReadyForDisplay: false, canRecover: true) == .recover)
    #expect(monitor.observe(now: 24, playerSeconds: 24, isPlaying: true, isReadyForDisplay: false, canRecover: true) == .none)
    #expect(monitor.observe(now: 45, playerSeconds: 45, isPlaying: true, isReadyForDisplay: false, canRecover: true) == .fail)
    #expect(!monitor.hasRenderedPlayback)
    #expect(monitor.observe(now: 46, playerSeconds: 46, isPlaying: true, isReadyForDisplay: true, canRecover: true) == .none)
    #expect(!monitor.hasRenderedPlayback) // Late evidence cannot revive terminal failure.
}

@Test func recoveryInstallationAndProgrammaticPauseDoNotResetStallDeadline() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    #expect(monitor.observe(now: 12, playerSeconds: 0, isPlaying: false, isReadyForDisplay: false, canRecover: true) == .recover)
    // HTTP success and replacement seek pause the player but do not prove recovery.
    #expect(monitor.observe(now: 40, playerSeconds: 0, isPlaying: false, isReadyForDisplay: true, canRecover: false) == .none)
    #expect(monitor.observe(now: 45, playerSeconds: 0, isPlaying: false, isReadyForDisplay: true, canRecover: false) == .fail)
}

@Test func midplayStallGetsOneRecoveryUntilVideoActuallyAdvances() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    #expect(monitor.observe(now: 5, playerSeconds: 5, isPlaying: true, isReadyForDisplay: true, canRecover: true) == .none)
    #expect(monitor.hasRenderedPlayback)
    #expect(monitor.observe(now: 17, playerSeconds: 5, isPlaying: false, isReadyForDisplay: true, canRecover: true) == .recover)
    #expect(monitor.observe(now: 30, playerSeconds: 0, isPlaying: false, isReadyForDisplay: false, canRecover: true) == .none)
    #expect(monitor.observe(now: 31, playerSeconds: 1, isPlaying: true, isReadyForDisplay: true, canRecover: true) == .none)
    // A later, independent stall has its own bounded recovery opportunity.
    #expect(monitor.observe(now: 43, playerSeconds: 1, isPlaying: false, isReadyForDisplay: true, canRecover: true) == .recover)
    #expect(monitor.observe(now: 76, playerSeconds: 1, isPlaying: false, isReadyForDisplay: true, canRecover: true) == .fail)
}

@Test func nonrecoveringPlatformsStillFailWithinTheSameBudget() {
    var monitor = PlaybackLiveness(now: 0, playerSeconds: 0)
    #expect(monitor.observe(now: 12, playerSeconds: 0, isPlaying: false, isReadyForDisplay: false, canRecover: false) == .none)
    #expect(monitor.observe(now: 45, playerSeconds: 0, isPlaying: false, isReadyForDisplay: false, canRecover: false) == .fail)
}
