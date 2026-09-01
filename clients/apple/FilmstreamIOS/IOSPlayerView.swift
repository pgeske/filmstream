import AVFoundation
import AVKit
import Combine
import FilmstreamCore
import Foundation
import MediaPlayer
import SwiftUI
import UIKit

struct IOSPlayerView: View {
    @Environment(\.dismiss) private var dismiss
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @Environment(\.verticalSizeClass) private var verticalSizeClass

    let movie: Movie
    let prepared: PreparedPlayback
    let api: FilmstreamAPI
    let nextEpisode: Episode?
    let onPlayNext: (@MainActor (Episode) async throws -> Void)?

    @StateObject private var controller: IOSPlaybackController
    @StateObject private var pictureInPicture = IOSPictureInPictureController()
    @State private var didClose = false
    @State private var didSaveEndProgress = false
    @State private var isChromeVisible = true
    @State private var scrubPosition: Double?
    @State private var isStartingNextEpisode = false
    @State private var nextEpisodeError: String?
    @State private var autoHideTask: Task<Void, Never>?

    init(
        movie: Movie,
        prepared: PreparedPlayback,
        api: FilmstreamAPI,
        nextEpisode: Episode? = nil,
        onPlayNext: (@MainActor (Episode) async throws -> Void)? = nil
    ) {
        self.movie = movie
        self.prepared = prepared
        self.api = api
        self.nextEpisode = nextEpisode
        self.onPlayNext = onPlayNext
        _controller = StateObject(
            wrappedValue: IOSPlaybackController(movie: movie, prepared: prepared, api: api)
        )
    }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            IOSPlayerSurface(
                player: controller.player,
                pictureInPicture: pictureInPicture
            )
            .ignoresSafeArea()
            .contentShape(Rectangle())
            .onTapGesture {
                toggleChromeVisibility()
            }
            .onContinuousHover { phase in
                if case .active = phase {
                    revealChrome()
                }
            }

            if let subtitle = controller.activeSubtitleText {
                VStack {
                    Spacer()
                    Text(subtitle)
                        .font(.system(
                            size: horizontalSizeClass == .regular ? 30 : 22,
                            weight: .semibold,
                            design: .rounded
                        ))
                        .foregroundStyle(.white)
                        .multilineTextAlignment(.center)
                        .lineSpacing(3)
                        .padding(.horizontal, 24)
                        .padding(
                            .bottom,
                            isChromeVisible
                                ? (verticalSizeClass == .compact ? 145 : 190)
                                : 28
                        )
                        .shadow(color: .black, radius: 3, x: 1, y: 1)
                        .shadow(color: .black.opacity(0.9), radius: 7)
                        .animation(.easeOut(duration: 0.2), value: isChromeVisible)
                }
                .allowsHitTesting(false)
            }

            chrome
                .opacity(isChromeVisible ? 1 : 0)
                .allowsHitTesting(isChromeVisible)
                .accessibilityHidden(!isChromeVisible)

            if let errorMessage = nextEpisodeError ?? controller.errorMessage {
                VStack(spacing: 12) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.largeTitle)
                        .foregroundStyle(Color.mobileTeaAmber)
                    Text(nextEpisodeError == nil ? "Unable to Play" : "Unable to Start Next Episode")
                        .font(.headline)
                    Text(errorMessage)
                        .font(.footnote)
                        .foregroundStyle(Color.mobileTeaMuted)
                        .multilineTextAlignment(.center)
                }
                .padding(24)
                .frame(maxWidth: 330)
                .background(.black.opacity(0.82), in: RoundedRectangle(cornerRadius: 18))
                .overlay {
                    RoundedRectangle(cornerRadius: 18, style: .continuous)
                        .stroke(Color.mobileTeaCream.opacity(0.12), lineWidth: 1)
                }
            } else if isStartingNextEpisode {
                VStack(spacing: 12) {
                    IOSPlaybackLoadingIndicator(size: 46)
                    Text("Starting \(nextEpisode?.label ?? "Next Episode")…")
                        .font(.headline)
                }
                .padding(24)
                .background(.black.opacity(0.82), in: RoundedRectangle(cornerRadius: 18))
            } else if controller.isWaiting {
                IOSPlaybackLoadingIndicator(size: 54)
                    .padding(20)
                    .background(.black.opacity(0.58), in: Circle())
                    .allowsHitTesting(false)
            }
        }
        .statusBarHidden(true)
        .persistentSystemOverlays(.hidden)
        .onAppear {
            UIApplication.shared.isIdleTimerDisabled = true
            controller.activateMediaSession()
            controller.play()
            scheduleAutoHide()
        }
        .onDisappear {
            UIApplication.shared.isIdleTimerDisabled = false
            autoHideTask?.cancel()
            closePlayback()
        }
        .onChange(of: controller.isPlaying) { _, _ in
            synchronizeChrome()
        }
        .onChange(of: controller.isWaiting) { _, _ in
            synchronizeChrome()
        }
        .onChange(of: controller.isSeeking) { _, _ in
            synchronizeChrome()
        }
        .onChange(of: controller.didReachEnd) { _, didReachEnd in
            if didReachEnd, nextEpisode != nil, onPlayNext != nil {
                startNextEpisode()
            }
        }
        .alert(
            "Unable to Start Picture in Picture",
            isPresented: Binding(
                get: { pictureInPicture.errorMessage != nil },
                set: { if !$0 { pictureInPicture.clearError() } }
            )
        ) {
            Button("OK") {
                pictureInPicture.clearError()
            }
        } message: {
            Text(pictureInPicture.errorMessage ?? "Picture in Picture is unavailable.")
        }
        .task {
            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: .seconds(15))
                } catch {
                    break
                }
                if !isStartingNextEpisode {
                    await reportProgress()
                }
            }
        }
    }

    private var chrome: some View {
        VStack(spacing: 0) {
            topBar
            Spacer()
            bottomBar
        }
    }

    private var topBar: some View {
        HStack(spacing: 14) {
            Button {
                closePlayback()
                dismiss()
            } label: {
                Image(systemName: "xmark")
                    .font(.headline.weight(.bold))
                    .frame(width: 44, height: 44)
                    .background(.black.opacity(0.58), in: Circle())
                    .overlay {
                        Circle()
                            .stroke(Color.white.opacity(0.14), lineWidth: 1)
                    }
            }
            .buttonStyle(MobileCardButtonStyle())
            .keyboardShortcut(.cancelAction)
            .accessibilityLabel("Close player")

            Text(movie.title)
                .font(.headline)
                .lineLimit(1)

            Spacer()

            Button {
                pictureInPicture.toggle()
                revealChrome()
            } label: {
                Image(systemName: pictureInPicture.isActive ? "pip.exit" : "pip.enter")
                    .font(.headline)
                    .frame(width: 44, height: 44)
                    .background(.black.opacity(0.58), in: Circle())
                    .overlay {
                        Circle()
                            .stroke(Color.white.opacity(0.14), lineWidth: 1)
                    }
            }
            .buttonStyle(MobileCardButtonStyle())
            .disabled(!pictureInPicture.isPossible && !pictureInPicture.isActive)
            .accessibilityLabel(
                pictureInPicture.isActive ? "Exit Picture in Picture" : "Picture in Picture"
            )

            Menu {
                if controller.subtitleOptions.isEmpty {
                    Button("No compatible text subtitles in this release") {}
                        .disabled(true)
                } else {
                    Button {
                        controller.selectSubtitle(nil)
                        revealChrome()
                    } label: {
                        subtitleLabel("Off", selected: controller.selectedSubtitle == nil)
                    }
                    Divider()
                    ForEach(controller.subtitleOptions) { track in
                        Button {
                            controller.selectSubtitle(track)
                            revealChrome()
                        } label: {
                            subtitleLabel(
                                track.mobileDisplayName,
                                selected: controller.selectedSubtitle?.index == track.index
                            )
                        }
                    }
                }
            } label: {
                Image(systemName: "captions.bubble")
                    .font(.headline)
                    .frame(width: 44, height: 44)
                    .background(.black.opacity(0.58), in: Circle())
                    .overlay {
                        Circle()
                            .stroke(Color.white.opacity(0.14), lineWidth: 1)
                    }
            }
            .buttonStyle(MobileCardButtonStyle())
            .accessibilityLabel("Subtitles")
        }
        .foregroundStyle(.white)
        .padding(.horizontal, 16)
        .padding(.top, 10)
        .padding(.bottom, 30)
        .background(
            LinearGradient(
                colors: [.black.opacity(0.82), .clear],
                startPoint: .top,
                endPoint: .bottom
            )
        )
    }

    private var bottomBar: some View {
        VStack(alignment: .leading, spacing: 13) {
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text(controller.stateLabel)
                        .font(.subheadline.weight(.semibold))
                    if let subtitle = controller.selectedSubtitle {
                        Text(subtitle.mobileDisplayName)
                            .font(.caption)
                            .foregroundStyle(.white.opacity(0.62))
                            .lineLimit(1)
                    }
                }
                Spacer()
            }

            Slider(
                value: Binding(
                    get: { scrubPosition ?? controller.positionSeconds },
                    set: { scrubPosition = $0 }
                ),
                in: 0...max(controller.durationSeconds, 1),
                onEditingChanged: handleScrubbing
            )
            .tint(Color.mobileTeaAccent)

            HStack {
                Text(formatTime(scrubPosition ?? controller.positionSeconds))
                Spacer()
                Text(formatTime(controller.durationSeconds))
            }
            .font(.caption.monospacedDigit())
            .foregroundStyle(.white.opacity(0.72))

            HStack(spacing: 30) {
                Spacer()
                playerButton(systemImage: "gobackward.30", label: "Back 30 seconds") {
                    controller.jump(by: -30)
                    revealChrome()
                }
                .keyboardShortcut(.leftArrow, modifiers: [])

                playerButton(
                    systemImage: controller.isPlaying ? "pause.fill" : "play.fill",
                    label: controller.isPlaying ? "Pause" : "Play",
                    prominent: true
                ) {
                    controller.togglePlayback()
                    revealChrome()
                }
                .keyboardShortcut(.space, modifiers: [])

                playerButton(systemImage: "goforward.30", label: "Forward 30 seconds") {
                    controller.jump(by: 30)
                    revealChrome()
                }
                .keyboardShortcut(.rightArrow, modifiers: [])
                Spacer()
            }
        }
        .foregroundStyle(.white)
        .padding(.horizontal, 20)
        .padding(.top, 42)
        .padding(.bottom, 16)
        .frame(maxWidth: 1_050)
        .frame(maxWidth: .infinity)
        .background(
            LinearGradient(
                colors: [.clear, .black.opacity(0.88)],
                startPoint: .top,
                endPoint: .bottom
            )
        )
    }

    private func playerButton(
        systemImage: String,
        label: String,
        prominent: Bool = false,
        action: @escaping () -> Void
    ) -> some View {
        Button(action: action) {
            Image(systemName: systemImage)
                .font(.system(size: prominent ? 25 : 21, weight: .semibold))
                .frame(width: prominent ? 58 : 48, height: prominent ? 58 : 48)
                .background(
                    prominent ? Color.mobileTeaAccent : Color.white.opacity(0.14),
                    in: Circle()
                )
                .overlay {
                    Circle()
                        .stroke(Color.white.opacity(prominent ? 0.1 : 0.16), lineWidth: 1)
                }
                .shadow(color: .black.opacity(0.24), radius: 8, y: 4)
                .foregroundStyle(prominent ? Color.mobileTeaBackground : .white)
        }
        .buttonStyle(MobileCardButtonStyle())
        .accessibilityLabel(label)
    }

    private func subtitleLabel(_ title: String, selected: Bool) -> some View {
        HStack {
            Text(title)
            if selected {
                Image(systemName: "checkmark")
            }
        }
    }

    private func handleScrubbing(_ isEditing: Bool) {
        if isEditing {
            autoHideTask?.cancel()
            if scrubPosition == nil {
                scrubPosition = controller.positionSeconds
            }
            return
        }
        guard let target = scrubPosition else { return }
        scrubPosition = nil
        controller.seek(to: target)
        revealChrome(autoHide: false)
    }

    private func toggleChromeVisibility() {
        autoHideTask?.cancel()
        if isChromeVisible {
            withAnimation(.easeOut(duration: 0.2)) {
                isChromeVisible = false
            }
        } else {
            revealChrome()
        }
    }

    private func revealChrome(autoHide: Bool = true) {
        autoHideTask?.cancel()
        withAnimation(.easeOut(duration: 0.18)) {
            isChromeVisible = true
        }
        if autoHide {
            scheduleAutoHide()
        }
    }

    private func scheduleAutoHide() {
        autoHideTask?.cancel()
        guard isChromeVisible,
              controller.isPlaying,
              !controller.isWaiting,
              !controller.isSeeking else {
            return
        }
        autoHideTask = Task { @MainActor in
            do {
                try await Task.sleep(for: .seconds(4))
            } catch {
                return
            }
            guard controller.isPlaying, !controller.isWaiting, !controller.isSeeking else { return }
            withAnimation(.easeOut(duration: 0.25)) {
                isChromeVisible = false
            }
            autoHideTask = nil
        }
    }

    private func synchronizeChrome() {
        if controller.isSeeking || (!controller.isPlaying && !controller.isWaiting) {
            revealChrome(autoHide: false)
        } else if isChromeVisible {
            scheduleAutoHide()
        }
    }

    private func startNextEpisode() {
        guard !isStartingNextEpisode,
              let nextEpisode,
              let onPlayNext else {
            return
        }
        isStartingNextEpisode = true
        nextEpisodeError = nil
        controller.pause()
        revealChrome(autoHide: false)

        Task { @MainActor in
            didSaveEndProgress = await reportProgress()
            do {
                try await onPlayNext(nextEpisode)
            } catch {
                isStartingNextEpisode = false
                nextEpisodeError = error.localizedDescription
            }
        }
    }

    private func closePlayback() {
        guard !didClose else { return }
        didClose = true
        let position = controller.positionSeconds
        let duration = controller.durationSeconds
        let activeSubtitle = controller.selectedSubtitle
        // Autoplay already saved completion; a second update would start a duplicate prewarm.
        let shouldSaveProgress = !didSaveEndProgress
        controller.stop()
        Task {
            if shouldSaveProgress, position > 0, duration > 0 {
                _ = try? await api.updateProgress(
                    for: movie,
                    positionSeconds: position,
                    durationSeconds: duration,
                    activeSubtitle: activeSubtitle
                )
            }
            try? await api.stopNativePlayback(prepared.playback.id)
        }
    }

    @discardableResult
    private func reportProgress() async -> Bool {
        guard !controller.isSeeking,
              controller.positionSeconds > 0,
              controller.durationSeconds > 0 else {
            return false
        }
        do {
            _ = try await api.updateProgress(
                for: movie,
                positionSeconds: controller.positionSeconds,
                durationSeconds: controller.durationSeconds,
                activeSubtitle: controller.selectedSubtitle
            )
            return true
        } catch {
            return false
        }
    }

    private func formatTime(_ seconds: Double) -> String {
        guard seconds.isFinite, seconds > 0 else { return "0:00" }
        let total = Int(seconds)
        let hours = total / 3600
        let minutes = total % 3600 / 60
        let remainingSeconds = total % 60
        if hours > 0 {
            return String(format: "%d:%02d:%02d", hours, minutes, remainingSeconds)
        }
        return String(format: "%d:%02d", minutes, remainingSeconds)
    }
}

private struct IOSPlaybackLoadingIndicator: View {
    let size: CGFloat
    @State private var isRotating = false

    var body: some View {
        Circle()
            .trim(from: 0.08, to: 0.82)
            .stroke(
                Color.mobileTeaAccent,
                style: StrokeStyle(lineWidth: max(4, size * 0.1), lineCap: .round)
            )
            .frame(width: size, height: size)
            .rotationEffect(.degrees(isRotating ? 360 : 0))
            .shadow(color: .black.opacity(0.6), radius: 10)
            .animation(
                .linear(duration: 0.9).repeatForever(autoreverses: false),
                value: isRotating
            )
            .onAppear {
                isRotating = true
            }
            .accessibilityLabel("Buffering")
    }
}

private struct IOSPlayerSurface: UIViewRepresentable {
    let player: AVPlayer
    let pictureInPicture: IOSPictureInPictureController

    func makeUIView(context: Context) -> IOSPlayerSurfaceView {
        let view = IOSPlayerSurfaceView()
        view.player = player
        view.pictureInPicture = pictureInPicture
        pictureInPicture.attach(to: view.playerLayer)
        return view
    }

    func updateUIView(_ view: IOSPlayerSurfaceView, context: Context) {
        if view.player !== player {
            view.player = player
        }
        view.pictureInPicture = pictureInPicture
        pictureInPicture.attach(to: view.playerLayer)
    }

    static func dismantleUIView(_ view: IOSPlayerSurfaceView, coordinator: Void) {
        view.pictureInPicture?.detach(from: view.playerLayer)
        view.player = nil
        view.pictureInPicture = nil
    }
}

private final class IOSPlayerSurfaceView: UIView {
    override class var layerClass: AnyClass { AVPlayerLayer.self }

    weak var pictureInPicture: IOSPictureInPictureController?

    var player: AVPlayer? {
        get { playerLayer.player }
        set { playerLayer.player = newValue }
    }

    var playerLayer: AVPlayerLayer {
        layer as! AVPlayerLayer
    }

    override init(frame: CGRect) {
        super.init(frame: frame)
        backgroundColor = .black
        playerLayer.videoGravity = .resizeAspect
    }

    required init?(coder: NSCoder) {
        fatalError("init(coder:) has not been implemented")
    }
}

@MainActor
private final class IOSPictureInPictureController: NSObject, ObservableObject {
    @Published private(set) var isPossible = false
    @Published private(set) var isActive = false
    @Published private(set) var errorMessage: String?

    private weak var playerLayer: AVPlayerLayer?
    private var controller: AVPictureInPictureController?
    private var possibleObservation: NSKeyValueObservation?

    func attach(to playerLayer: AVPlayerLayer) {
        guard self.playerLayer !== playerLayer else { return }
        detach()
        self.playerLayer = playerLayer

        guard AVPictureInPictureController.isPictureInPictureSupported() else {
            isPossible = false
            return
        }

        let contentSource = AVPictureInPictureController.ContentSource(playerLayer: playerLayer)
        let controller = AVPictureInPictureController(contentSource: contentSource)
        controller.delegate = self
        controller.canStartPictureInPictureAutomaticallyFromInline = true
        self.controller = controller
        possibleObservation = controller.observe(
            \.isPictureInPicturePossible,
            options: [.initial, .new]
        ) { [weak self] controller, _ in
            let isPossible = controller.isPictureInPicturePossible
            Task { @MainActor in
                self?.isPossible = isPossible
            }
        }
    }

    func toggle() {
        guard let controller else { return }
        errorMessage = nil
        if controller.isPictureInPictureActive {
            controller.stopPictureInPicture()
        } else if controller.isPictureInPicturePossible {
            controller.startPictureInPicture()
        }
    }

    func detach(from playerLayer: AVPlayerLayer? = nil) {
        guard playerLayer == nil || self.playerLayer === playerLayer else { return }
        if controller?.isPictureInPictureActive == true {
            controller?.stopPictureInPicture()
        }
        possibleObservation?.invalidate()
        possibleObservation = nil
        controller?.delegate = nil
        controller?.contentSource = nil
        controller = nil
        self.playerLayer = nil
        isPossible = false
        isActive = false
    }

    func clearError() {
        errorMessage = nil
    }
}

extension IOSPictureInPictureController: @preconcurrency AVPictureInPictureControllerDelegate {
    func pictureInPictureControllerDidStartPictureInPicture(
        _ pictureInPictureController: AVPictureInPictureController
    ) {
        isActive = true
    }

    func pictureInPictureControllerDidStopPictureInPicture(
        _ pictureInPictureController: AVPictureInPictureController
    ) {
        isActive = false
    }

    func pictureInPictureController(
        _ pictureInPictureController: AVPictureInPictureController,
        failedToStartPictureInPictureWithError error: any Error
    ) {
        isActive = false
        errorMessage = error.localizedDescription
    }

    func pictureInPictureController(
        _ pictureInPictureController: AVPictureInPictureController,
        restoreUserInterfaceForPictureInPictureStopWithCompletionHandler completionHandler: @escaping (Bool) -> Void
    ) {
        completionHandler(true)
    }
}

private extension HLSSubtitleTrack {
    var mobileDisplayName: String {
        let languageName: String
        if let language, !language.isEmpty {
            languageName = Locale.current.localizedString(forLanguageCode: language)?.capitalized
                ?? language.uppercased()
        } else {
            languageName = "Unknown Language"
        }
        if let title, !title.isEmpty {
            return "\(languageName) (\(title))"
        }
        if isForced == true {
            return "\(languageName) (Forced)"
        }
        return languageName
    }
}

@MainActor
private final class IOSPlaybackController: ObservableObject {
    let player = AVPlayer()

    @Published private(set) var positionSeconds: Double
    @Published private(set) var durationSeconds: Double
    @Published private(set) var isPlaying = false
    @Published private(set) var isSeeking = false
    @Published private(set) var isWaiting = true
    @Published private(set) var didReachEnd = false
    @Published private(set) var stateLabel = "Preparing Stream…"
    @Published private(set) var errorMessage: String?
    @Published private(set) var subtitleOptions: [HLSSubtitleTrack]
    @Published private(set) var selectedSubtitle: HLSSubtitleTrack?
    @Published private(set) var activeSubtitleText: String?

    private let api: FilmstreamAPI
    private let movie: Movie
    private let playback: Playback
    private var timeline: HLSPlaybackTimeline
    private var burnedSubtitleIndex: Int?
    private var timeObserver: Any?
    private var statusObservation: NSKeyValueObservation?
    private var playbackObservation: NSKeyValueObservation?
    private var itemEndObserver: NSObjectProtocol?
    private var remoteCommandTargets: [(MPRemoteCommand, Any)] = []
    private var seekTask: Task<Void, Never>?
    private var subtitleTask: Task<Void, Never>?
    private var subtitleSwitchTask: Task<Void, Never>?
    private var playbackFailureTask: Task<Void, Never>?
    private var subtitleCues: [SubtitleCue] = []
    private var seekGeneration = 0
    private var seekOriginSeconds: Double
    private var pendingSeekSeconds: Double?
    private var resumeAfterSeek = true
    private var wantsToPlay = false
    private var stopped = false
    private var mediaSessionIsActive = false
    private var mediaSessionID: String?
    private var lastNowPlayingSecond = -1

    private static var activeMediaSessionID: String?

    init(movie: Movie, prepared: PreparedPlayback, api: FilmstreamAPI) {
        self.api = api
        self.movie = movie
        playback = prepared.playback
        timeline = prepared.hls.timeline
        positionSeconds = timeline.requestedSeconds
        durationSeconds = max(0, prepared.hls.durationSeconds ?? 0)
        seekOriginSeconds = timeline.requestedSeconds
        subtitleOptions = prepared.hls.subtitles ?? []
        selectedSubtitle = Self.preferredSubtitle(in: subtitleOptions)
        burnedSubtitleIndex = prepared.hls.burnedSubtitleIndex

        player.automaticallyWaitsToMinimizeStalling = true
        player.actionAtItemEnd = .pause
        installItem(url: prepared.hls.playlistURL)
        let initialPlayerSeconds = timeline.playerSeconds(
            forMediaSeconds: timeline.requestedSeconds
        )
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
        ) { [weak self] time in
            Task { @MainActor in
                guard let self, !self.isSeeking else { return }
                let current = time.seconds
                if current.isFinite, current >= 0 {
                    self.positionSeconds = min(
                        self.timeline.mediaSeconds(forPlayerSeconds: current),
                        self.durationSeconds > 0 ? self.durationSeconds : .greatestFiniteMagnitude
                    )
                    self.updateActiveSubtitle()
                    let elapsedSecond = Int(self.positionSeconds)
                    if elapsedSecond / 5 != self.lastNowPlayingSecond / 5 {
                        self.lastNowPlayingSecond = elapsedSecond
                        self.publishNowPlayingInfo()
                    }
                }
            }
        }

        restartSubtitleUpdates()

        playbackObservation = player.observe(\.timeControlStatus, options: [.initial, .new]) { [weak self] player, _ in
            Task { @MainActor in
                guard let self, !self.stopped, !self.isSeeking else { return }
                switch player.timeControlStatus {
                case .playing:
                    self.cancelPlaybackFailure()
                    self.wantsToPlay = true
                    self.isPlaying = true
                    self.isWaiting = false
                    self.stateLabel = "Playing"
                case .waitingToPlayAtSpecifiedRate:
                    self.isPlaying = self.wantsToPlay
                    self.isWaiting = true
                    self.stateLabel = "Buffering…"
                    self.schedulePlaybackFailure()
                case .paused:
                    self.cancelPlaybackFailure()
                    if self.subtitleSwitchTask == nil {
                        self.wantsToPlay = false
                    }
                    self.isPlaying = false
                    self.isWaiting = false
                    self.stateLabel = "Paused"
                @unknown default:
                    self.isWaiting = true
                    self.stateLabel = "Preparing Stream…"
                }
                self.publishNowPlayingInfo()
            }
        }
    }

    func play() {
        guard !stopped else { return }
        errorMessage = nil
        wantsToPlay = true
        isPlaying = true
        didReachEnd = false
        player.play()
        publishNowPlayingInfo()
    }

    func pause() {
        guard !stopped else { return }
        wantsToPlay = false
        isPlaying = false
        player.pause()
        publishNowPlayingInfo()
    }

    func activateMediaSession() {
        guard !mediaSessionIsActive else { return }
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
        mediaSessionIsActive = true
        publishNowPlayingInfo()
    }

    func togglePlayback() {
        guard !stopped else { return }
        if isSeeking {
            resumeAfterSeek.toggle()
            isPlaying = resumeAfterSeek
            return
        }
        if player.timeControlStatus == .playing || (wantsToPlay && player.timeControlStatus != .paused) {
            pause()
        } else {
            play()
        }
    }

    private func publishNowPlayingInfo() {
        guard mediaSessionIsActive else { return }
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
    }

    private func deactivateMediaSession() {
        guard mediaSessionIsActive else { return }
        for (command, target) in remoteCommandTargets {
            command.removeTarget(target)
        }
        remoteCommandTargets.removeAll()
        mediaSessionIsActive = false
        let sessionID = mediaSessionID
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
        try? AVAudioSession.sharedInstance().setActive(
            false,
            options: .notifyOthersOnDeactivation
        )
    }

    func jump(by seconds: Double) {
        guard !stopped, durationSeconds > 0 else { return }
        let origin = pendingSeekSeconds ?? positionSeconds
        seek(to: origin + seconds)
    }

    func seek(to requestedSeconds: Double) {
        guard !stopped, durationSeconds > 0 else { return }
        let target = min(max(0, requestedSeconds), max(0, durationSeconds - 1))
        if !isSeeking {
            seekOriginSeconds = positionSeconds
            resumeAfterSeek = wantsToPlay
        }
        pendingSeekSeconds = target
        positionSeconds = target
        isSeeking = true
        isWaiting = true
        stateLabel = "Seeking…"
        errorMessage = nil
        player.pause()

        seekGeneration += 1
        let generation = seekGeneration
        seekTask?.cancel()
        seekTask = Task { [weak self] in
            do {
                try await Task.sleep(for: .milliseconds(450))
                guard !Task.isCancelled else { return }
                await self?.performSeek(to: target, generation: generation)
            } catch {
                return
            }
        }
    }

    func selectSubtitle(_ track: HLSSubtitleTrack?) {
        selectedSubtitle = track
        HLSSubtitleTrack.savePreference(track)
        switchSubtitleStreamIfNeeded()
    }

    private func schedulePlaybackFailure() {
        guard playbackFailureTask == nil, wantsToPlay, !stopped else { return }
        playbackFailureTask = Task { @MainActor [weak self] in
            do {
                try await Task.sleep(for: NativePlaybackConfiguration.stallTimeout)
            } catch {
                return
            }
            guard let self,
                  !self.stopped,
                  self.wantsToPlay,
                  self.player.timeControlStatus != .playing else {
                return
            }
            self.wantsToPlay = false
            self.isPlaying = false
            self.isWaiting = false
            self.stateLabel = "Unable to Play Stream"
            self.errorMessage = "The stream did not begin playing. Close the player and try this episode again."
            self.player.pause()
            self.playbackFailureTask = nil
        }
    }

    private func cancelPlaybackFailure() {
        playbackFailureTask?.cancel()
        playbackFailureTask = nil
    }

    func stop() {
        guard !stopped else { return }
        stopped = true
        seekGeneration += 1
        seekTask?.cancel()
        seekTask = nil
        subtitleTask?.cancel()
        subtitleTask = nil
        subtitleSwitchTask?.cancel()
        subtitleSwitchTask = nil
        cancelPlaybackFailure()
        deactivateMediaSession()
        if let itemEndObserver {
            NotificationCenter.default.removeObserver(itemEndObserver)
            self.itemEndObserver = nil
        }
        player.pause()
        player.replaceCurrentItem(with: nil)
        if let timeObserver {
            player.removeTimeObserver(timeObserver)
            self.timeObserver = nil
        }
        statusObservation?.invalidate()
        statusObservation = nil
        playbackObservation?.invalidate()
        playbackObservation = nil
    }

    private func performSeek(to target: Double, generation: Int) async {
        guard generation == seekGeneration, !stopped else { return }
        let playerTarget = timeline.playerSeconds(forMediaSeconds: target)
        if canSeekLocally(to: playerTarget) {
            let time = CMTime(seconds: playerTarget, preferredTimescale: 600)
            player.seek(to: time, toleranceBefore: .zero, toleranceAfter: .zero) { [weak self] finished in
                Task { @MainActor in
                    guard finished else { return }
                    self?.finishSeek(generation: generation)
                }
            }
            return
        }

        do {
            let prepared = try await api.prepareNativePlayback(playback, startSeconds: target)
            guard !Task.isCancelled, generation == seekGeneration, !stopped else { return }
            timeline = prepared.hls.timeline
            burnedSubtitleIndex = prepared.hls.burnedSubtitleIndex
            updateSubtitleOptions(prepared.hls.subtitles ?? [])
            if let duration = prepared.hls.durationSeconds, duration > 0 {
                durationSeconds = duration
            }
            positionSeconds = target
            installItem(url: cacheBusted(prepared.hls.playlistURL))
            let playerPosition = timeline.playerSeconds(forMediaSeconds: target)
            player.seek(
                to: CMTime(seconds: playerPosition, preferredTimescale: 600),
                toleranceBefore: .zero,
                toleranceAfter: .zero
            ) { [weak self] finished in
                Task { @MainActor in
                    guard finished else { return }
                    self?.finishSeek(generation: generation)
                }
            }
        } catch {
            guard generation == seekGeneration, !stopped else { return }
            pendingSeekSeconds = nil
            positionSeconds = seekOriginSeconds
            isSeeking = false
            isWaiting = false
            isPlaying = false
            wantsToPlay = false
            stateLabel = "Seek Failed"
            errorMessage = error.localizedDescription
            seekTask = nil
        }
    }

    private func finishSeek(generation: Int) {
        guard generation == seekGeneration, !stopped else { return }
        pendingSeekSeconds = nil
        isSeeking = false
        seekTask = nil
        if resumeAfterSeek {
            play()
        } else {
            wantsToPlay = false
            isPlaying = false
            isWaiting = false
            stateLabel = "Paused"
        }
    }

    private func canSeekLocally(to seconds: Double) -> Bool {
        guard seconds >= 0, let item = player.currentItem else { return false }
        let target = CMTime(seconds: seconds, preferredTimescale: 600)
        return item.seekableTimeRanges.contains { value in
            CMTimeRangeContainsTime(value.timeRangeValue, time: target)
        }
    }

    private func updateSubtitleOptions(_ tracks: [HLSSubtitleTrack]) {
        let previous = selectedSubtitle
        subtitleOptions = tracks
        if let previous {
            selectedSubtitle = tracks.first(where: { $0.index == previous.index })
                ?? tracks.first(where: {
                    $0.language == previous.language && $0.title == previous.title
                })
        }
        restartSubtitleUpdates()
    }

    private func switchSubtitleStreamIfNeeded() {
        let desiredBitmapIndex = selectedSubtitle?.isBitmap == true ? selectedSubtitle?.index : nil
        guard desiredBitmapIndex != burnedSubtitleIndex else {
            restartSubtitleUpdates()
            return
        }

        subtitleSwitchTask?.cancel()
        subtitleTask?.cancel()
        subtitleTask = nil
        subtitleCues = []
        activeSubtitleText = nil
        let resumePosition = max(0, positionSeconds)
        let shouldResume = wantsToPlay
        player.pause()
        isWaiting = true
        stateLabel = "Changing Subtitles…"
        errorMessage = nil

        subtitleSwitchTask = Task { @MainActor [weak self] in
            guard let self else { return }
            do {
                let prepared = try await self.api.prepareNativePlayback(
                    self.playback,
                    startSeconds: resumePosition,
                    bitmapSubtitleIndex: desiredBitmapIndex,
                    useSavedSubtitlePreference: false
                )
                guard !Task.isCancelled, !self.stopped else { return }
                self.timeline = prepared.hls.timeline
                self.burnedSubtitleIndex = prepared.hls.burnedSubtitleIndex
                self.updateSubtitleOptions(prepared.hls.subtitles ?? [])
                self.installItem(url: self.cacheBusted(prepared.hls.playlistURL))
                let playerPosition = self.timeline.playerSeconds(
                    forMediaSeconds: resumePosition
                )
                self.player.seek(
                    to: CMTime(seconds: playerPosition, preferredTimescale: 600),
                    toleranceBefore: .zero,
                    toleranceAfter: .zero
                ) { [weak self] finished in
                    Task { @MainActor in
                        guard let self, !self.stopped else { return }
                        self.subtitleSwitchTask = nil
                        self.isWaiting = false
                        guard finished else {
                            self.wantsToPlay = false
                            self.isPlaying = false
                            self.stateLabel = "Paused"
                            return
                        }
                        if shouldResume {
                            self.play()
                        } else {
                            self.stateLabel = "Paused"
                        }
                    }
                }
            } catch {
                guard !Task.isCancelled, !self.stopped else { return }
                self.subtitleSwitchTask = nil
                self.isWaiting = false
                self.wantsToPlay = false
                self.isPlaying = false
                self.stateLabel = "Unable to Change Subtitles"
                self.errorMessage = error.localizedDescription
            }
        }
    }

    private func restartSubtitleUpdates() {
        subtitleTask?.cancel()
        subtitleTask = nil
        subtitleCues = []
        activeSubtitleText = nil
        guard let track = selectedSubtitle, !track.isBitmap, !stopped else { return }

        let subtitleTimeline = timeline
        subtitleTask = Task { [weak self] in
            guard let self else { return }
            do {
                try await self.api.startSubtitle(playbackID: self.playback.id, track: track)
            } catch {
                return
            }
            while !Task.isCancelled {
                guard !self.stopped,
                      self.selectedSubtitle?.index == track.index,
                      self.timeline == subtitleTimeline else {
                    return
                }
                do {
                    if let cues = try await self.api.subtitleCues(
                        playbackID: self.playback.id,
                        track: track
                    ) {
                        guard !Task.isCancelled,
                              self.selectedSubtitle?.index == track.index,
                              self.timeline == subtitleTimeline else {
                            return
                        }
                        self.subtitleCues = cues
                        self.updateActiveSubtitle()
                    }
                } catch {
                    // Subtitle polling is best-effort while the WebVTT file grows.
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
        let active = subtitleCues.filter {
            positionSeconds >= $0.startSeconds && positionSeconds <= $0.endSeconds
        }
        activeSubtitleText = active.isEmpty ? nil : active.map(\.text).joined(separator: "\n")
    }

    private static func preferredSubtitle(in tracks: [HLSSubtitleTrack]) -> HLSSubtitleTrack? {
        HLSSubtitleTrack.savedPreference(in: tracks)
    }

    private func installItem(url: URL) {
        statusObservation?.invalidate()
        if let itemEndObserver {
            NotificationCenter.default.removeObserver(itemEndObserver)
            self.itemEndObserver = nil
        }
        didReachEnd = false
        let item = AVPlayerItem(url: url)
        NativePlaybackConfiguration.configure(item)
        statusObservation = item.observe(\.status, options: [.initial, .new]) { [weak self] item, _ in
            Task { @MainActor in
                guard let self, !self.stopped else { return }
                if item.status == .failed {
                    self.cancelPlaybackFailure()
                    self.isWaiting = false
                    self.isPlaying = false
                    self.wantsToPlay = false
                    self.stateLabel = "Unable to Play Stream"
                    self.errorMessage = item.error?.localizedDescription ?? "The HLS stream could not be opened."
                }
            }
        }
        itemEndObserver = NotificationCenter.default.addObserver(
            forName: .AVPlayerItemDidPlayToEndTime,
            object: item,
            queue: .main
        ) { [weak self] _ in
            Task { @MainActor in
                guard let self, !self.stopped else { return }
                self.positionSeconds = max(self.positionSeconds, self.durationSeconds)
                self.wantsToPlay = false
                self.isPlaying = false
                self.isWaiting = false
                self.didReachEnd = true
                self.stateLabel = "Episode Finished"
                self.publishNowPlayingInfo()
            }
        }
        player.replaceCurrentItem(with: item)
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
}
