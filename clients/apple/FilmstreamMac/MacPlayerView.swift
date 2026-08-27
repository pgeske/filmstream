import AppKit
import AVFoundation
import AVKit
import Combine
import FilmstreamCore
import Foundation
import SwiftUI

struct MacPlayerView: View {
    let movie: Movie
    let prepared: PreparedPlayback
    let api: FilmstreamAPI
    let nextEpisode: Episode?
    let onPlayNext: (@MainActor (Episode) async throws -> Void)?
    let onClose: () -> Void

    @StateObject private var controller: MacPlaybackController
    @StateObject private var pictureInPicture = MacPictureInPictureController()
    @State private var didClose = false
    @State private var didSaveEndProgress = false
    @State private var scrubPosition: Double?
    @State private var isFullScreen = false
    @State private var controlsAreVisible = true
    @State private var isStartingNextEpisode = false
    @State private var nextEpisodeError: String?
    @State private var controlsHideTask: Task<Void, Never>?
    @State private var keyEventMonitor: Any?

    init(
        movie: Movie,
        prepared: PreparedPlayback,
        api: FilmstreamAPI,
        nextEpisode: Episode? = nil,
        onPlayNext: (@MainActor (Episode) async throws -> Void)? = nil,
        onClose: @escaping () -> Void
    ) {
        self.movie = movie
        self.prepared = prepared
        self.api = api
        self.nextEpisode = nextEpisode
        self.onPlayNext = onPlayNext
        self.onClose = onClose
        _controller = StateObject(
            wrappedValue: MacPlaybackController(prepared: prepared, api: api)
        )
    }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            MacAVPlayerView(
                player: controller.player,
                pictureInPicture: pictureInPicture,
                onPointerActivity: revealControls
            )
            .ignoresSafeArea()

            VStack(spacing: 0) {
                playerHeader
                    .opacity(controlsAreVisible ? 1 : 0)
                    .allowsHitTesting(controlsAreVisible)
                    .accessibilityHidden(!controlsAreVisible)

                Spacer()

                if let subtitle = controller.activeSubtitleText {
                    Text(subtitle)
                        .font(.system(size: 28, weight: .semibold, design: .rounded))
                        .foregroundStyle(.white)
                        .multilineTextAlignment(.center)
                        .lineSpacing(4)
                        .frame(maxWidth: 1_000)
                        .padding(.horizontal, 54)
                        .padding(.bottom, 32)
                        .shadow(color: .black, radius: 3, x: 2, y: 2)
                        .shadow(color: .black.opacity(0.9), radius: 7)
                        .allowsHitTesting(false)
                }

                playbackControls
                    .opacity(controlsAreVisible ? 1 : 0)
                    .allowsHitTesting(controlsAreVisible)
                    .accessibilityHidden(!controlsAreVisible)
            }
            .animation(.easeOut(duration: 0.2), value: controlsAreVisible)

            if let errorMessage = nextEpisodeError ?? controller.errorMessage {
                VStack(spacing: 14) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.largeTitle)
                        .foregroundStyle(Color.macTeaAmber)
                    Text(nextEpisodeError == nil ? "Unable to Play" : "Unable to Start Next Episode")
                        .font(.headline)
                    Text(errorMessage)
                        .font(.callout)
                        .foregroundStyle(Color.macTeaMuted)
                        .multilineTextAlignment(.center)
                        .frame(maxWidth: 420)
                }
                .padding(28)
                .background(.black.opacity(0.8), in: RoundedRectangle(cornerRadius: 18))
            } else if isStartingNextEpisode {
                VStack(spacing: 14) {
                    ProgressView()
                        .controlSize(.large)
                        .tint(Color.macTeaAccent)
                    Text("Starting \(nextEpisode?.label ?? "Next Episode")…")
                        .font(.headline)
                }
                .padding(28)
                .background(.black.opacity(0.8), in: RoundedRectangle(cornerRadius: 18))
            } else if controller.isWaiting {
                ProgressView()
                    .controlSize(.large)
                    .tint(Color.macTeaAccent)
                    .padding(24)
                    .background(.black.opacity(0.56), in: Circle())
                    .allowsHitTesting(false)
            }
        }
        .onAppear {
            syncFullScreenState()
            installPlayerKeyMonitor()
            controller.play()
            revealControls()
        }
        .onDisappear {
            controlsHideTask?.cancel()
            removePlayerKeyMonitor()
            closePlayback()
        }
        .onChange(of: controller.isPlaying) { _, isPlaying in
            if isPlaying {
                revealControls()
            } else {
                keepControlsVisible()
            }
        }
        .onChange(of: controller.errorMessage) { _, errorMessage in
            if errorMessage != nil {
                keepControlsVisible()
            }
        }
        .onChange(of: controller.didReachEnd) { _, didReachEnd in
            if didReachEnd, nextEpisode != nil, onPlayNext != nil {
                startNextEpisode()
            }
        }
        .onExitCommand {
            if activeWindow?.styleMask.contains(.fullScreen) == true {
                toggleFullScreen()
            } else {
                requestClose()
            }
        }
        .onReceive(NotificationCenter.default.publisher(for: NSWindow.didEnterFullScreenNotification)) { _ in
            isFullScreen = true
        }
        .onReceive(NotificationCenter.default.publisher(for: NSWindow.didExitFullScreenNotification)) { _ in
            isFullScreen = false
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

    private var playerHeader: some View {
        HStack(spacing: 14) {
            VStack(alignment: .leading, spacing: 3) {
                Text(movie.title)
                    .font(.headline.weight(.semibold))
                    .lineLimit(1)
                Text("\(controller.stateLabel)  •  \(formatTime(controller.positionSeconds)) / \(formatTime(controller.durationSeconds))")
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.white.opacity(0.64))
            }

            Spacer()

            if !controller.subtitleOptions.isEmpty {
                Menu {
                    Button {
                        revealControls()
                        controller.selectSubtitle(nil)
                    } label: {
                        subtitleMenuLabel("Off", selected: controller.selectedSubtitle == nil)
                    }
                    Divider()
                    ForEach(controller.subtitleOptions) { track in
                        Button {
                            revealControls()
                            controller.selectSubtitle(track)
                        } label: {
                            subtitleMenuLabel(
                                track.macDisplayName,
                                selected: controller.selectedSubtitle?.index == track.index
                            )
                        }
                    }
                } label: {
                    Label(
                        controller.selectedSubtitle?.macDisplayName ?? "Subtitles",
                        systemImage: "captions.bubble"
                    )
                }
                .menuStyle(.borderlessButton)
                .fixedSize()
            }

            Button {
                revealControls()
                pictureInPicture.toggle()
            } label: {
                Label(
                    pictureInPicture.isActive ? "Exit Picture in Picture" : "Picture in Picture",
                    systemImage: pictureInPicture.isActive ? "pip.exit" : "pip.enter"
                )
            }
            .buttonStyle(.borderless)
            .disabled(!pictureInPicture.isPossible && !pictureInPicture.isActive)
            .help(
                pictureInPicture.isPossible || pictureInPicture.isActive
                    ? "Picture in Picture"
                    : "Picture in Picture becomes available when the video is ready"
            )

            Button {
                revealControls()
                toggleFullScreen()
            } label: {
                Label(
                    isFullScreen ? "Exit Full Screen" : "Full Screen",
                    systemImage: "arrow.up.left.and.arrow.down.right"
                )
            }
            .buttonStyle(.borderless)
            .help("Full Screen (F or double-click the video)")

            Button(action: requestClose) {
                Label("Close", systemImage: "xmark.circle.fill")
            }
            .buttonStyle(.borderless)
            .keyboardShortcut(.cancelAction)
        }
        .padding(.horizontal, 20)
        .padding(.vertical, 14)
        .foregroundStyle(.white)
        .background(
            LinearGradient(
                colors: [.black.opacity(0.82), .black.opacity(0.18)],
                startPoint: .top,
                endPoint: .bottom
            )
        )
    }

    private var playbackControls: some View {
        VStack(spacing: 10) {
            Slider(
                value: Binding(
                    get: { scrubPosition ?? controller.positionSeconds },
                    set: { scrubPosition = $0 }
                ),
                in: 0...max(controller.durationSeconds, 1),
                onEditingChanged: handleScrubbing
            )
            .tint(Color.macTeaAccent)

            HStack(spacing: 18) {
                Text(formatTime(scrubPosition ?? controller.positionSeconds))
                    .frame(width: 72, alignment: .leading)

                Spacer()

                Button {
                    revealControls()
                    controller.jump(by: -30)
                } label: {
                    Label("Back 30 Seconds", systemImage: "gobackward.30")
                        .labelStyle(.iconOnly)
                }
                .help("Back 30 Seconds")

                Button {
                    revealControls()
                    controller.togglePlayback()
                } label: {
                    Label(
                        controller.isPlaying ? "Pause" : "Play",
                        systemImage: controller.isPlaying ? "pause.fill" : "play.fill"
                    )
                    .labelStyle(.iconOnly)
                    .font(.title2)
                    .frame(width: 34)
                }
                .keyboardShortcut(.space, modifiers: [])

                Button {
                    revealControls()
                    controller.jump(by: 30)
                } label: {
                    Label("Forward 30 Seconds", systemImage: "goforward.30")
                        .labelStyle(.iconOnly)
                }
                .help("Forward 30 Seconds")

                Spacer()

                Text(formatTime(controller.durationSeconds))
                    .frame(width: 72, alignment: .trailing)
            }
            .font(.callout.monospacedDigit())
            .buttonStyle(.borderless)
        }
        .padding(.horizontal, 22)
        .padding(.top, 34)
        .padding(.bottom, 16)
        .foregroundStyle(.white)
        .background(
            LinearGradient(
                colors: [.clear, .black.opacity(0.84)],
                startPoint: .top,
                endPoint: .bottom
            )
        )
    }

    private func handleScrubbing(_ isEditing: Bool) {
        if isEditing {
            keepControlsVisible()
            if scrubPosition == nil {
                scrubPosition = controller.positionSeconds
            }
            return
        }
        guard let target = scrubPosition else { return }
        scrubPosition = nil
        controller.seek(to: target)
        revealControls()
    }

    private func subtitleMenuLabel(_ title: String, selected: Bool) -> some View {
        HStack {
            Text(title)
            if selected {
                Image(systemName: "checkmark")
            }
        }
    }

    private func revealControls() {
        controlsHideTask?.cancel()
        controlsHideTask = nil
        if !controlsAreVisible {
            withAnimation(.easeOut(duration: 0.2)) {
                controlsAreVisible = true
            }
        }
        guard controller.isPlaying else { return }

        controlsHideTask = Task {
            do {
                try await Task.sleep(for: .seconds(3))
            } catch {
                return
            }
            guard controller.isPlaying, scrubPosition == nil else { return }
            withAnimation(.easeOut(duration: 0.25)) {
                controlsAreVisible = false
            }
            NSCursor.setHiddenUntilMouseMoves(true)
            controlsHideTask = nil
        }
    }

    private func keepControlsVisible() {
        controlsHideTask?.cancel()
        controlsHideTask = nil
        if !controlsAreVisible {
            withAnimation(.easeOut(duration: 0.2)) {
                controlsAreVisible = true
            }
        }
    }

    private func requestClose() {
        if isFullScreen {
            toggleFullScreen()
        }
        closePlayback()
        onClose()
    }

    private var activeWindow: NSWindow? {
        NSApplication.shared.keyWindow ?? NSApplication.shared.mainWindow
    }

    private func toggleFullScreen() {
        activeWindow?.toggleFullScreen(nil)
    }

    private func syncFullScreenState() {
        isFullScreen = activeWindow?.styleMask.contains(.fullScreen) == true
    }

    private func installPlayerKeyMonitor() {
        guard keyEventMonitor == nil else { return }
        keyEventMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
            guard let window = NSApplication.shared.keyWindow ?? NSApplication.shared.mainWindow else {
                return event
            }

            if event.keyCode == 53, window.styleMask.contains(.fullScreen) {
                window.toggleFullScreen(nil)
                return nil
            }

            let modifiers = event.modifierFlags.intersection([.command, .control, .option, .shift])
            if !event.isARepeat,
               modifiers.isEmpty,
               event.charactersIgnoringModifiers?.lowercased() == "f" {
                window.toggleFullScreen(nil)
                return nil
            }

            return event
        }
    }

    private func removePlayerKeyMonitor() {
        guard let keyEventMonitor else { return }
        NSEvent.removeMonitor(keyEventMonitor)
        self.keyEventMonitor = nil
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
        keepControlsVisible()

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

// AppKit avoids a SwiftUI VideoPlayer bridge crash on current macOS releases.
// TeaStream supplies movie controls because growing HLS playlists otherwise appear live to AVKit.
private struct MacAVPlayerView: NSViewRepresentable {
    let player: AVPlayer
    let pictureInPicture: MacPictureInPictureController
    let onPointerActivity: () -> Void

    func makeNSView(context: Context) -> MacInteractivePlayerView {
        let playerView = MacInteractivePlayerView()
        playerView.player = player
        playerView.pictureInPicture = pictureInPicture
        playerView.onPointerActivity = onPointerActivity
        pictureInPicture.attach(to: playerView.playerLayer)
        return playerView
    }

    func updateNSView(_ playerView: MacInteractivePlayerView, context: Context) {
        if playerView.player !== player {
            playerView.player = player
        }
        playerView.pictureInPicture = pictureInPicture
        playerView.onPointerActivity = onPointerActivity
        pictureInPicture.attach(to: playerView.playerLayer)
    }

    static func dismantleNSView(_ playerView: MacInteractivePlayerView, coordinator: Void) {
        playerView.pictureInPicture?.detach(from: playerView.playerLayer)
        playerView.player = nil
        playerView.pictureInPicture = nil
        playerView.onPointerActivity = nil
    }
}

private final class MacInteractivePlayerView: NSView {
    var onPointerActivity: (() -> Void)?
    weak var pictureInPicture: MacPictureInPictureController?
    private var pointerTrackingArea: NSTrackingArea?

    var playerLayer: AVPlayerLayer {
        guard let playerLayer = layer as? AVPlayerLayer else {
            preconditionFailure("MacInteractivePlayerView must use AVPlayerLayer")
        }
        return playerLayer
    }

    var player: AVPlayer? {
        get { playerLayer.player }
        set { playerLayer.player = newValue }
    }

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        wantsLayer = true
        playerLayer.videoGravity = .resizeAspect
        let doubleClickRecognizer = NSClickGestureRecognizer(
            target: self,
            action: #selector(handleDoubleClick)
        )
        doubleClickRecognizer.numberOfClicksRequired = 2
        addGestureRecognizer(doubleClickRecognizer)
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        fatalError("init(coder:) has not been implemented")
    }

    override func makeBackingLayer() -> CALayer {
        AVPlayerLayer()
    }

    override func layout() {
        super.layout()
        playerLayer.frame = bounds
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        window?.acceptsMouseMovedEvents = true
    }

    override func updateTrackingAreas() {
        if let pointerTrackingArea {
            removeTrackingArea(pointerTrackingArea)
        }
        let trackingArea = NSTrackingArea(
            rect: .zero,
            options: [.mouseMoved, .mouseEnteredAndExited, .activeInKeyWindow, .inVisibleRect],
            owner: self
        )
        addTrackingArea(trackingArea)
        pointerTrackingArea = trackingArea
        super.updateTrackingAreas()
    }

    override func mouseEntered(with event: NSEvent) {
        onPointerActivity?()
        super.mouseEntered(with: event)
    }

    override func mouseMoved(with event: NSEvent) {
        onPointerActivity?()
        super.mouseMoved(with: event)
    }

    override func mouseDown(with event: NSEvent) {
        onPointerActivity?()
        super.mouseDown(with: event)
    }

    @objc private func handleDoubleClick() {
        onPointerActivity?()
        window?.toggleFullScreen(nil)
    }
}

@MainActor
private final class MacPictureInPictureController: NSObject, ObservableObject {
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

extension MacPictureInPictureController: @preconcurrency AVPictureInPictureControllerDelegate {
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
        NSApplication.shared.activate(ignoringOtherApps: true)
        completionHandler(true)
    }
}

private extension HLSSubtitleTrack {
    var macDisplayName: String {
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
private final class MacPlaybackController: ObservableObject {
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
    private let playback: Playback
    private var streamStartSeconds: Double
    private var burnedSubtitleIndex: Int?
    private var timeObserver: Any?
    private var statusObservation: NSKeyValueObservation?
    private var playbackObservation: NSKeyValueObservation?
    private var itemEndObserver: NSObjectProtocol?
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

    init(prepared: PreparedPlayback, api: FilmstreamAPI) {
        self.api = api
        playback = prepared.playback
        streamStartSeconds = max(0, prepared.hls.startSeconds)
        positionSeconds = streamStartSeconds
        durationSeconds = max(0, prepared.hls.durationSeconds ?? 0)
        seekOriginSeconds = streamStartSeconds
        subtitleOptions = prepared.hls.subtitles ?? []
        selectedSubtitle = Self.preferredSubtitle(in: subtitleOptions)
        burnedSubtitleIndex = prepared.hls.burnedSubtitleIndex

        player.automaticallyWaitsToMinimizeStalling = true
        player.actionAtItemEnd = .pause
        installItem(url: prepared.hls.playlistURL)

        timeObserver = player.addPeriodicTimeObserver(
            forInterval: CMTime(seconds: 0.25, preferredTimescale: 600),
            queue: .main
        ) { [weak self] time in
            Task { @MainActor in
                guard let self, !self.isSeeking else { return }
                let current = time.seconds
                if current.isFinite, current >= 0 {
                    self.positionSeconds = min(
                        max(0, self.streamStartSeconds + current),
                        self.durationSeconds > 0 ? self.durationSeconds : .greatestFiniteMagnitude
                    )
                    self.updateActiveSubtitle()
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
                    self.isPlaying = false
                    self.isWaiting = false
                    self.stateLabel = "Paused"
                @unknown default:
                    self.isWaiting = true
                    self.stateLabel = "Preparing Stream…"
                }
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
    }

    func pause() {
        guard !stopped else { return }
        wantsToPlay = false
        isPlaying = false
        player.pause()
    }

    func togglePlayback() {
        guard !stopped else { return }
        if isSeeking {
            resumeAfterSeek.toggle()
            isPlaying = resumeAfterSeek
            return
        }
        if wantsToPlay {
            pause()
        } else {
            play()
        }
    }

    func jump(by seconds: Double) {
        guard !stopped, durationSeconds > 0 else { return }
        let origin = pendingSeekSeconds ?? positionSeconds
        seek(to: origin + seconds)
    }

    func seek(to requestedSeconds: Double) {
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
        let localTarget = target - streamStartSeconds
        if canSeekLocally(to: localTarget) {
            let time = CMTime(seconds: localTarget, preferredTimescale: 600)
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
            streamStartSeconds = max(0, prepared.hls.startSeconds)
            burnedSubtitleIndex = prepared.hls.burnedSubtitleIndex
            updateSubtitleOptions(prepared.hls.subtitles ?? [])
            if let duration = prepared.hls.durationSeconds, duration > 0 {
                durationSeconds = duration
            }
            positionSeconds = streamStartSeconds
            installItem(url: cacheBusted(prepared.hls.playlistURL))
            finishSeek(generation: generation)
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
                self.streamStartSeconds = max(0, prepared.hls.startSeconds)
                self.burnedSubtitleIndex = prepared.hls.burnedSubtitleIndex
                self.updateSubtitleOptions(prepared.hls.subtitles ?? [])
                self.installItem(url: self.cacheBusted(prepared.hls.playlistURL))
                let localPosition = max(0, resumePosition - self.streamStartSeconds)
                self.player.seek(
                    to: CMTime(seconds: localPosition, preferredTimescale: 600),
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

        let offset = streamStartSeconds
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
                      self.streamStartSeconds == offset else {
                    return
                }
                do {
                    if let cues = try await self.api.subtitleCues(
                        playbackID: self.playback.id,
                        track: track,
                        offsetSeconds: offset
                    ) {
                        guard !Task.isCancelled,
                              self.selectedSubtitle?.index == track.index,
                              self.streamStartSeconds == offset else {
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
