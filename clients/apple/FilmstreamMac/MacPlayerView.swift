import AppKit
import AVFoundation
import AVKit
import Combine
import FilmstreamCore
import Foundation
import SwiftUI

struct MacPlayerView: View {
    let movie: Movie
    let api: FilmstreamAPI
    let nextEpisode: Episode?
    let onPlayNext: (@MainActor (Episode) async throws -> Void)?
    let onClose: () -> Void

    @StateObject private var controller: NativePlaybackController
    @StateObject private var pictureInPicture = MacPictureInPictureController()
    @State private var didClose = false
    @State private var nextEpisodeTask: Task<Void, Never>?
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
        self.api = api
        self.nextEpisode = nextEpisode
        self.onPlayNext = onPlayNext
        self.onClose = onClose
        _controller = StateObject(
            wrappedValue: NativePlaybackController(movie: movie, prepared: prepared, api: api)
        )
    }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            MacAVPlayerView(
                controller: controller,
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
                VStack(spacing: 16) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.largeTitle)
                        .foregroundStyle(Color.macTeaAmber)
                    Text(nextEpisodeError == nil ? "Unable to Play" : "Unable to Start Next Episode")
                        .font(.title.weight(.bold))
                        .foregroundStyle(Color.macTeaCream)
                        .multilineTextAlignment(.center)
                    Text(errorMessage)
                        .font(.title3)
                        .foregroundStyle(Color.macTeaCream)
                        .multilineTextAlignment(.center)
                        .lineLimit(6)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: 520)
                    HStack(spacing: 12) {
                        Button(action: retryAfterError) {
                            Label("Try Again", systemImage: "arrow.clockwise")
                        }
                        .buttonStyle(MacDetailButtonStyle(kind: .prominent))

                        Button(action: requestClose) {
                            Label("Close", systemImage: "xmark")
                        }
                        .buttonStyle(MacDetailButtonStyle(kind: .standard))
                    }
                    .padding(.top, 6)
                }
                .padding(32)
                .background(.black.opacity(0.86), in: RoundedRectangle(cornerRadius: 20))
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
                VStack(spacing: 16) {
                    ProgressView()
                        .controlSize(.large)
                        .tint(Color.macTeaAccent)
                        .padding(24)
                        .background(.black.opacity(0.56), in: Circle())

                    if let waitingDetail = controller.waitingDetail {
                        Text(waitingDetail)
                            .font(.title3.weight(.semibold))
                            .foregroundStyle(Color.macTeaCream)
                            .multilineTextAlignment(.center)
                            .lineLimit(2)
                            .frame(maxWidth: 560)
                            .padding(.horizontal, 18)
                            .padding(.vertical, 10)
                            .background(
                                .black.opacity(0.72),
                                in: RoundedRectangle(cornerRadius: 14, style: .continuous)
                            )
                    }
                }
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

            if controller.audioOptions.count > 1 {
                Menu {
                    ForEach(controller.audioOptions) { track in
                        Button {
                            revealControls()
                            controller.selectAudio(track)
                        } label: {
                            trackMenuLabel(
                                track.displayName,
                                selected: controller.selectedAudio?.index == track.index
                            )
                        }
                    }
                } label: {
                    Label(
                        controller.selectedAudio?.displayName ?? "Audio",
                        systemImage: "waveform"
                    )
                }
                .menuStyle(.borderlessButton)
                .fixedSize()
            }

            if !controller.subtitleOptions.isEmpty {
                Menu {
                    Button {
                        revealControls()
                        controller.selectSubtitle(nil)
                    } label: {
                        trackMenuLabel("Off", selected: controller.selectedSubtitle == nil)
                    }
                    Divider()
                    ForEach(controller.subtitleOptions) { track in
                        Button {
                            revealControls()
                            controller.selectSubtitle(track)
                        } label: {
                            trackMenuLabel(
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

    private func trackMenuLabel(_ title: String, selected: Bool) -> some View {
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

    private func retryAfterError() {
        if nextEpisodeError != nil {
            startNextEpisode()
        } else {
            controller.retry()
        }
        revealControls()
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

        nextEpisodeTask = Task { @MainActor in
            didSaveEndProgress = await reportProgress()
            do {
                try Task.checkCancellation()
                try await onPlayNext(nextEpisode)
            } catch {
                guard !Task.isCancelled, !didClose else { return }
                isStartingNextEpisode = false
                nextEpisodeError = error.localizedDescription
            }
        }
    }

    private func closePlayback() {
        guard !didClose else { return }
        didClose = true
        nextEpisodeTask?.cancel()
        nextEpisodeTask = nil
        let position = controller.positionSeconds
        let duration = controller.durationSeconds
        let activeSubtitle = controller.selectedSubtitle
        // Recovery may replace the playback the view was opened with.
        let playbackID = controller.playbackID
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
            try? await api.stopNativePlayback(playbackID)
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
    let controller: NativePlaybackController
    private var player: AVPlayer { controller.player }
    let pictureInPicture: MacPictureInPictureController
    let onPointerActivity: () -> Void

    func makeNSView(context: Context) -> MacInteractivePlayerView {
        let playerView = MacInteractivePlayerView()
        playerView.player = player
        controller.attachVideoLayer(playerView.playerLayer)
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
