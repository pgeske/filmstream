import AVFoundation
import AVKit
import Combine
import FilmstreamCore
import Foundation
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

    @StateObject private var controller: NativePlaybackController
    @StateObject private var pictureInPicture = IOSPictureInPictureController()
    @State private var didClose = false
    @State private var nextEpisodeTask: Task<Void, Never>?
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
            wrappedValue: NativePlaybackController(movie: movie, prepared: prepared, api: api)
        )
    }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            IOSPlayerSurface(
                controller: controller,
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
                VStack(spacing: 14) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.largeTitle)
                        .foregroundStyle(Color.mobileTeaAmber)
                        .accessibilityHidden(true)
                    Text(nextEpisodeError == nil ? "Unable to Play" : "Unable to Start Next Episode")
                        .font(.title2.weight(.bold))
                        .foregroundStyle(Color.mobileTeaCream)
                        .multilineTextAlignment(.center)
                    Text(errorMessage)
                        .font(.body.weight(.medium))
                        .foregroundStyle(Color.mobileTeaCream)
                        .multilineTextAlignment(.center)
                        .lineLimit(5)
                    Button {
                        if nextEpisodeError != nil {
                            startNextEpisode()
                        } else {
                            controller.retry()
                        }
                    } label: {
                        Label("Try Again", systemImage: "arrow.clockwise")
                    }
                    .buttonStyle(MobileDetailButtonStyle(kind: .prominent))
                    .keyboardShortcut(.defaultAction)
                    .padding(.top, 4)
                    Button {
                        closePlayback()
                        dismiss()
                    } label: {
                        Label("Close Player", systemImage: "xmark")
                    }
                    .buttonStyle(MobileDetailButtonStyle(kind: .standard))
                }
                .padding(24)
                .frame(maxWidth: 420)
                .background(.black.opacity(0.86), in: RoundedRectangle(cornerRadius: 18))
                .overlay {
                    RoundedRectangle(cornerRadius: 18, style: .continuous)
                        .stroke(Color.mobileTeaCream.opacity(0.18), lineWidth: 1)
                }
                .padding(.horizontal, 20)
            } else if isStartingNextEpisode {
                VStack(spacing: 12) {
                    IOSPlaybackLoadingIndicator(size: 46)
                    Text("Starting \(nextEpisode?.label ?? "Next Episode")…")
                        .font(.headline)
                }
                .padding(24)
                .background(.black.opacity(0.82), in: RoundedRectangle(cornerRadius: 18))
            } else if controller.isWaiting {
                VStack(spacing: 14) {
                    IOSPlaybackLoadingIndicator(size: 54)
                        .padding(20)
                        .background(.black.opacity(0.58), in: Circle())
                    if let waitingDetail = controller.waitingDetail {
                        Text(waitingDetail)
                            .font(.headline)
                            .foregroundStyle(Color.mobileTeaCream)
                            .multilineTextAlignment(.center)
                            .lineLimit(2)
                            .padding(.horizontal, 16)
                            .padding(.vertical, 10)
                            .background(
                                .black.opacity(0.72),
                                in: RoundedRectangle(cornerRadius: 14, style: .continuous)
                            )
                            .frame(maxWidth: 440)
                    }
                }
                .padding(.horizontal, 24)
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

            if controller.audioOptions.count > 1 {
                Menu {
                    ForEach(controller.audioOptions) { track in
                        Button {
                            controller.selectAudio(track)
                            revealChrome()
                        } label: {
                            trackLabel(
                                track.displayName,
                                selected: controller.selectedAudio?.index == track.index
                            )
                        }
                    }
                } label: {
                    Image(systemName: "waveform")
                        .font(.headline)
                        .frame(width: 44, height: 44)
                        .background(.black.opacity(0.58), in: Circle())
                        .overlay {
                            Circle()
                                .stroke(Color.white.opacity(0.14), lineWidth: 1)
                        }
                }
                .buttonStyle(MobileCardButtonStyle())
                .accessibilityLabel("Audio")
            }

            Menu {
                if controller.subtitleOptions.isEmpty {
                    Button("No compatible text subtitles in this release") {}
                        .disabled(true)
                } else {
                    Button {
                        controller.selectSubtitle(nil)
                        revealChrome()
                    } label: {
                        trackLabel("Off", selected: controller.selectedSubtitle == nil)
                    }
                    Divider()
                    ForEach(controller.subtitleOptions) { track in
                        Button {
                            controller.selectSubtitle(track)
                            revealChrome()
                        } label: {
                            trackLabel(
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

    private func trackLabel(_ title: String, selected: Bool) -> some View {
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
        // Recovery may have replaced the original playback with a new server session.
        let playbackID = controller.playbackID
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
    let controller: NativePlaybackController
    private var player: AVPlayer { controller.player }
    let pictureInPicture: IOSPictureInPictureController

    func makeUIView(context: Context) -> IOSPlayerSurfaceView {
        let view = IOSPlayerSurfaceView()
        view.player = player
        controller.attachVideoLayer(view.playerLayer)
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
