import AVFoundation
import Combine
import FilmstreamCore
import Foundation
import MediaPlayer
import SwiftUI
import UIKit

struct PlayerView: View {
    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var scenePhase

    let movie: Movie
    let prepared: PreparedPlayback
    let api: FilmstreamAPI
    let nextEpisode: Episode?
    let onPlayNext: (@MainActor (Episode) async throws -> Void)?

    @StateObject private var controller: NativePlaybackController
    @State private var didClose = false
    @State private var isPlaybackChromeVisible = true
    @State private var isSubtitlePickerPresented = false
    @State private var isStartingNextEpisode = false
    @State private var nextEpisodeError: String?
    @State private var chromeAutoHideTask: Task<Void, Never>?
    @FocusState private var receivesRemoteCommands: Bool
    @FocusState private var focusedPlaybackAction: PlaybackAction?

    private enum PlaybackAction: Hashable {
        case nextEpisode
    }

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
        ZStack(alignment: .bottom) {
            Color.black.ignoresSafeArea()
            NativePlayerSurface(player: controller.player)
                .ignoresSafeArea()

            if isPlaybackChromePresented {
                LinearGradient(
                    colors: [.clear, .black.opacity(0.86)],
                    startPoint: .top,
                    endPoint: .bottom
                )
                .frame(height: 270)
                .transition(.opacity)
                .allowsHitTesting(false)
            }

            if let subtitle = controller.activeSubtitleText {
                Text(subtitle)
                    .font(.system(size: 44, weight: .semibold, design: .rounded))
                    .foregroundStyle(.white)
                    .multilineTextAlignment(.center)
                    .lineSpacing(5)
                    .frame(maxWidth: 1_500)
                    .padding(.horizontal, 80)
                    .padding(.bottom, isPlaybackChromePresented ? 245 : 82)
                    .shadow(color: .black, radius: 3, x: 2, y: 2)
                    .shadow(color: .black.opacity(0.9), radius: 7)
                    .transition(.opacity)
                    .animation(.easeOut(duration: 0.22), value: isPlaybackChromePresented)
                    .allowsHitTesting(false)
            }

            if isPlaybackChromePresented {
                controls
                    .padding(.horizontal, 68)
                    .padding(.bottom, 42)
                    .transition(.opacity.combined(with: .move(edge: .bottom)))
            }

            if let errorMessage = nextEpisodeError ?? controller.errorMessage {
                VStack(spacing: 18) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.largeTitle)
                        .foregroundStyle(Color.teaAmber)
                    Text(nextEpisodeError == nil ? "Unable to Play" : "Unable to Start Next Episode")
                        .font(.headline)
                    Text(errorMessage)
                        .font(.footnote)
                        .foregroundStyle(Color.teaMuted)
                        .lineLimit(2)
                }
                .padding(30)
                .background(.black.opacity(0.76), in: RoundedRectangle(cornerRadius: 20))
                .allowsHitTesting(false)
            } else if isStartingNextEpisode {
                VStack(spacing: 18) {
                    PlaybackLoadingIndicator()
                    Text("Starting \(nextEpisode?.label ?? "Next Episode")…")
                        .font(.headline.weight(.semibold))
                        .foregroundStyle(Color.teaCream)
                }
                .padding(30)
                .background(.black.opacity(0.76), in: RoundedRectangle(cornerRadius: 20))
                .allowsHitTesting(false)
            } else if controller.isWaiting {
                PlaybackLoadingIndicator()
                    .allowsHitTesting(false)
            }

            if isSubtitlePickerPresented {
                SubtitlePicker(
                    tracks: controller.subtitleOptions,
                    selected: controller.selectedSubtitle,
                    onSelect: { track in
                        controller.selectSubtitle(track)
                        Task { @MainActor in
                            try? await Task.sleep(for: .milliseconds(100))
                            closeSubtitlePicker()
                        }
                    },
                    onDismiss: closeSubtitlePicker
                )
                .transition(.opacity.combined(with: .move(edge: .trailing)))
            }
        }
        .focusable(!isSubtitlePickerPresented)
        .focused($receivesRemoteCommands)
        .onAppear {
            receivesRemoteCommands = true
            controller.activateMediaSession()
            controller.play()
        }
        .onDisappear {
            chromeAutoHideTask?.cancel()
            closePlayback()
        }
        .onChange(of: scenePhase) { _, phase in
            switch phase {
            case .active:
                Task { await controller.reconnectAfterInterruption() }
            case .inactive, .background:
                controller.markInterrupted()
            @unknown default:
                break
            }
        }
        .onTapGesture {
            guard !isSubtitlePickerPresented else { return }
            if isPlaybackChromeVisible {
                controller.togglePlayback()
            }
            revealPlaybackChrome()
        }
        .onPlayPauseCommand {
            guard !isSubtitlePickerPresented else { return }
            controller.togglePlayback()
            revealPlaybackChrome()
        }
        .onMoveCommand { direction in
            guard !isSubtitlePickerPresented else { return }
            switch direction {
            case .left:
                revealPlaybackChrome(autoHide: false)
                controller.jump(by: -30)
            case .right:
                revealPlaybackChrome(autoHide: false)
                controller.jump(by: 30)
            case .up:
                guard !controller.subtitleOptions.isEmpty else {
                    revealPlaybackChrome()
                    return
                }
                revealPlaybackChrome(autoHide: false)
                receivesRemoteCommands = false
                withAnimation(.easeOut(duration: 0.2)) {
                    isSubtitlePickerPresented = true
                }
            case .down:
                guard nextEpisode != nil, onPlayNext != nil else {
                    revealPlaybackChrome()
                    return
                }
                revealPlaybackChrome(autoHide: false)
                receivesRemoteCommands = false
                focusedPlaybackAction = .nextEpisode
            default:
                break
            }
        }
        .onChange(of: controller.isPlaying) { _, _ in
            synchronizePlaybackChrome()
        }
        .onChange(of: controller.isWaiting) { _, _ in
            synchronizePlaybackChrome()
        }
        .onChange(of: controller.isSeeking) { _, _ in
            synchronizePlaybackChrome()
        }
        .onChange(of: controller.didReachEnd) { _, didReachEnd in
            if didReachEnd, nextEpisode != nil, onPlayNext != nil {
                startNextEpisode()
            }
        }
        .onChange(of: focusedPlaybackAction) { _, action in
            if action == nil, !isSubtitlePickerPresented {
                receivesRemoteCommands = true
                schedulePlaybackChromeAutoHide()
            } else {
                revealPlaybackChrome(autoHide: false)
            }
        }
        .onExitCommand {
            if isSubtitlePickerPresented {
                closeSubtitlePicker()
            } else {
                closePlayback()
                dismiss()
            }
        }
        .task {
            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: .seconds(15))
                } catch {
                    break
                }
                await reportProgress()
            }
        }
    }

    private var isPlaybackChromePresented: Bool {
        isPlaybackChromeVisible || isSubtitlePickerPresented
    }

    private var controls: some View {
        VStack(alignment: .leading, spacing: 15) {
            HStack {
                Text(movie.title)
                    .font(.title2.weight(.bold))
                Spacer()
                if let nextEpisode, onPlayNext != nil {
                    Button {
                        startNextEpisode()
                    } label: {
                        Label("Next \(nextEpisode.label)", systemImage: "forward.end.fill")
                            .font(.headline.weight(.bold))
                            .foregroundStyle(Color.teaCream)
                            .padding(.horizontal, 18)
                            .padding(.vertical, 12)
                            .background(
                                focusedPlaybackAction == .nextEpisode
                                    ? Color.teaAccent
                                    : Color.teaPanelElevated,
                                in: Capsule()
                            )
                            .overlay {
                                Capsule()
                                    .stroke(Color.teaAccentLight.opacity(0.5), lineWidth: 1)
                            }
                    }
                    .buttonStyle(.plain)
                    .focusEffectDisabled()
                    .focused($focusedPlaybackAction, equals: .nextEpisode)
                    .disabled(isStartingNextEpisode)
                    .onMoveCommand { direction in
                        switch direction {
                        case .up:
                            focusedPlaybackAction = nil
                            receivesRemoteCommands = true
                        case .left:
                            controller.jump(by: -30)
                        case .right:
                            controller.jump(by: 30)
                        default:
                            break
                        }
                    }
                }
                VStack(alignment: .trailing, spacing: 7) {
                    Label(
                        controller.stateLabel,
                        systemImage: controller.isPlaying ? "pause.fill" : "play.fill"
                    )
                    Label(
                        controller.selectedSubtitle?.displayName
                            ?? (controller.subtitleOptions.isEmpty ? "No Text Subtitles" : "Subtitles Off"),
                        systemImage: "captions.bubble"
                    )
                    .font(.footnote)
                }
                .foregroundStyle(Color.teaMuted)
            }

            ProgressView(
                value: controller.positionSeconds,
                total: max(controller.durationSeconds, 1)
            )
            .tint(Color.teaAccent)

            HStack {
                Text(formatTime(controller.positionSeconds))
                Spacer()
                Text(formatTime(controller.durationSeconds))
            }
            .font(.footnote.monospacedDigit())
        }
    }

    private func revealPlaybackChrome(autoHide: Bool = true) {
        chromeAutoHideTask?.cancel()
        chromeAutoHideTask = nil
        withAnimation(.easeOut(duration: 0.22)) {
            isPlaybackChromeVisible = true
        }
        if autoHide {
            schedulePlaybackChromeAutoHide()
        }
    }

    private func schedulePlaybackChromeAutoHide() {
        chromeAutoHideTask?.cancel()
        chromeAutoHideTask = nil
        guard isPlaybackChromeVisible,
              !isSubtitlePickerPresented,
              focusedPlaybackAction == nil,
              !isStartingNextEpisode,
              controller.isPlaying,
              !controller.isWaiting,
              !controller.isSeeking else {
            return
        }
        chromeAutoHideTask = Task { @MainActor in
            do {
                try await Task.sleep(for: .seconds(4))
            } catch {
                return
            }
            guard controller.isPlaying,
                  !controller.isWaiting,
                  !controller.isSeeking,
                  !isSubtitlePickerPresented,
                  focusedPlaybackAction == nil,
                  !isStartingNextEpisode else {
                return
            }
            withAnimation(.easeOut(duration: 0.28)) {
                isPlaybackChromeVisible = false
            }
            chromeAutoHideTask = nil
        }
    }

    private func synchronizePlaybackChrome() {
        if controller.isSeeking || (!controller.isPlaying && !controller.isWaiting) {
            revealPlaybackChrome(autoHide: false)
        } else if isPlaybackChromeVisible {
            schedulePlaybackChromeAutoHide()
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
        focusedPlaybackAction = nil
        controller.pause()
        revealPlaybackChrome(autoHide: false)

        Task { @MainActor in
            await reportProgress()
            do {
                try await onPlayNext(nextEpisode)
            } catch {
                isStartingNextEpisode = false
                nextEpisodeError = error.localizedDescription
                if !controller.didReachEnd {
                    controller.play()
                }
            }
        }
    }

    private func closeSubtitlePicker() {
        withAnimation(.easeOut(duration: 0.18)) {
            isSubtitlePickerPresented = false
        }
        Task { @MainActor in
            receivesRemoteCommands = true
            schedulePlaybackChromeAutoHide()
        }
    }

    private func closePlayback() {
        guard !didClose else { return }
        didClose = true
        let position = controller.positionSeconds
        let duration = controller.durationSeconds
        controller.stop()
        Task {
            if position > 0, duration > 0 {
                _ = try? await api.updateProgress(
                    for: movie,
                    positionSeconds: position,
                    durationSeconds: duration
                )
            }
            try? await api.stopNativePlayback(controller.playbackID)
        }
    }

    private func reportProgress() async {
        guard !controller.isSeeking,
              controller.positionSeconds > 0,
              controller.durationSeconds > 0 else {
            return
        }
        _ = try? await api.updateProgress(
            for: movie,
            positionSeconds: controller.positionSeconds,
            durationSeconds: controller.durationSeconds
        )
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

private extension HLSSubtitleTrack {
    var displayName: String {
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

private struct SubtitleOptionButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .opacity(configuration.isPressed ? 0.82 : 1)
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
    }
}

private struct SubtitlePicker: View {
    let tracks: [HLSSubtitleTrack]
    let selected: HLSSubtitleTrack?
    let onSelect: (HLSSubtitleTrack?) -> Void
    let onDismiss: () -> Void

    @FocusState private var focusedOption: Int?

    private var listHeight: CGFloat {
        min(CGFloat(tracks.count + 1) * 76, 520)
    }

    var body: some View {
        ZStack(alignment: .trailing) {
            Color.black.opacity(0.34)
                .ignoresSafeArea()

            VStack(alignment: .leading, spacing: 20) {
                HStack(spacing: 14) {
                    Image(systemName: "captions.bubble.fill")
                        .foregroundStyle(Color.teaAccentLight)
                    Text("Subtitles")
                        .font(.system(size: 34, weight: .bold, design: .rounded))
                }

                Text("Choose a track")
                    .font(.headline)
                    .foregroundStyle(.white.opacity(0.55))

                ScrollView {
                    LazyVStack(spacing: 8) {
                        optionRow(id: -1, title: "Off", isSelected: selected == nil) {
                            onSelect(nil)
                        }
                        ForEach(tracks) { track in
                            optionRow(
                                id: track.index,
                                title: displayName(for: track),
                                isSelected: selected?.index == track.index
                            ) {
                                onSelect(track)
                            }
                        }
                    }
                }
                .frame(height: listHeight)
                .scrollIndicators(.hidden)
            }
            .padding(34)
            .frame(width: 570)
            .background(
                LinearGradient(
                    colors: [Color.teaPanelElevated, Color.teaBackground.opacity(0.98)],
                    startPoint: .topLeading,
                    endPoint: .bottomTrailing
                ),
                in: RoundedRectangle(cornerRadius: 26, style: .continuous)
            )
            .overlay {
                RoundedRectangle(cornerRadius: 26, style: .continuous)
                    .stroke(Color.teaAccent.opacity(0.2), lineWidth: 1)
            }
            .shadow(color: .black.opacity(0.6), radius: 38, x: -12)
            .padding(.trailing, 68)
        }
        .onExitCommand(perform: onDismiss)
        .task {
            focusedOption = selected?.index ?? -1
        }
    }

    private func displayName(for track: HLSSubtitleTrack) -> String {
        let matching = tracks.filter { $0.displayName == track.displayName }
        guard matching.count > 1,
              let position = matching.firstIndex(where: { $0.index == track.index }),
              position > 0 else {
            return track.displayName
        }
        return "\(track.displayName) \(position + 1)"
    }

    private func optionRow(
        id: Int,
        title: String,
        isSelected: Bool,
        action: @escaping () -> Void
    ) -> some View {
        Button(action: action) {
            HStack(spacing: 16) {
                Capsule()
                    .fill(focusedOption == id ? Color.teaAccent : .clear)
                    .frame(width: 4, height: 32)
                Text(title)
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(Color.teaCream)
                    .lineLimit(1)
                Spacer()
                if isSelected {
                    Image(systemName: "checkmark")
                        .font(.headline.weight(.bold))
                        .foregroundStyle(Color.teaAccentLight)
                }
            }
            .padding(.horizontal, 16)
            .frame(height: 66)
            .contentShape(RoundedRectangle(cornerRadius: 14, style: .continuous))
            .background(
                focusedOption == id ? Color.teaAccent.opacity(0.14) : Color.clear,
                in: RoundedRectangle(cornerRadius: 14, style: .continuous)
            )
            .overlay {
                if focusedOption == id {
                    RoundedRectangle(cornerRadius: 14, style: .continuous)
                        .stroke(Color.teaAccentLight.opacity(0.22), lineWidth: 1)
                }
            }
        }
        .buttonStyle(SubtitleOptionButtonStyle())
        .focusEffectDisabled()
        .focused($focusedOption, equals: id)
    }
}

private struct PlaybackLoadingIndicator: View {
    @State private var isRotating = false

    var body: some View {
        Circle()
            .trim(from: 0.08, to: 0.82)
            .stroke(
                Color.teaAccent,
                style: StrokeStyle(lineWidth: 7, lineCap: .round)
            )
            .frame(width: 68, height: 68)
            .rotationEffect(.degrees(isRotating ? 360 : 0))
            .shadow(color: .black.opacity(0.65), radius: 12)
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

private struct NativePlayerSurface: UIViewRepresentable {
    let player: AVPlayer

    func makeUIView(context: Context) -> PlayerViewSurface {
        let view = PlayerViewSurface()
        view.player = player
        return view
    }

    func updateUIView(_ view: PlayerViewSurface, context: Context) {
        view.player = player
    }
}

private final class PlayerViewSurface: UIView {
    override class var layerClass: AnyClass { AVPlayerLayer.self }

    var player: AVPlayer? {
        get { playerLayer.player }
        set { playerLayer.player = newValue }
    }

    private var playerLayer: AVPlayerLayer {
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
private final class NativePlaybackController: ObservableObject {
    private static var activeMediaSessionID: String?

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
    private var playback: Playback
    private var requestedStreamStartSeconds: Double
    private var streamStartSeconds: Double
    private var timeObserver: Any?
    private var statusObservation: NSKeyValueObservation?
    private var playbackObservation: NSKeyValueObservation?
    private var itemEndObserver: NSObjectProtocol?
    private var remoteCommandTargets: [(MPRemoteCommand, Any)] = []
    private var seekTask: Task<Void, Never>?
    private var subtitleTask: Task<Void, Never>?
    private var bufferingRecoveryTask: Task<Void, Never>?
    private var subtitleCues: [SubtitleCue] = []
    private var seekGeneration = 0
    private var seekOriginSeconds: Double
    private var pendingSeekSeconds: Double?
    private var resumeAfterSeek = true
    private var wantsToPlay = false
    private var stopped = false
    private var wasInterrupted = false
    private var isRecovering = false
    private var mediaSessionIsActive = false
    private var mediaSessionID: String?
    private var lastNowPlayingSecond = -1

    var playbackID: String { playback.id }

    init(movie: Movie, prepared: PreparedPlayback, api: FilmstreamAPI) {
        self.api = api
        self.movie = movie
        playback = prepared.playback
        requestedStreamStartSeconds = max(
            0,
            prepared.hls.requestedStartSeconds ?? prepared.hls.startSeconds
        )
        streamStartSeconds = max(0, prepared.hls.startSeconds)
        positionSeconds = requestedStreamStartSeconds
        durationSeconds = max(0, prepared.hls.durationSeconds ?? 0)
        seekOriginSeconds = requestedStreamStartSeconds
        subtitleOptions = prepared.hls.subtitles ?? []
        selectedSubtitle = Self.preferredSubtitle(in: subtitleOptions)

        player.automaticallyWaitsToMinimizeStalling = true
        player.actionAtItemEnd = .pause
        installItem(url: prepared.hls.playlistURL)
        let initialOffset = max(0, requestedStreamStartSeconds - streamStartSeconds)
        if initialOffset > 0 {
            player.seek(
                to: CMTime(seconds: initialOffset, preferredTimescale: 600),
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
                        max(0, self.streamStartSeconds + current),
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
                    self.bufferingRecoveryTask?.cancel()
                    self.bufferingRecoveryTask = nil
                    self.isPlaying = true
                    self.isWaiting = false
                    self.stateLabel = "Playing"
                case .waitingToPlayAtSpecifiedRate:
                    self.isPlaying = self.wantsToPlay
                    self.isWaiting = true
                    self.stateLabel = self.isRecovering ? "Reconnecting…" : "Buffering…"
                    self.scheduleBufferingRecovery()
                case .paused:
                    self.bufferingRecoveryTask?.cancel()
                    self.bufferingRecoveryTask = nil
                    self.isPlaying = false
                    self.isWaiting = self.isRecovering
                    self.stateLabel = self.isRecovering ? "Reconnecting…" : "Paused"
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
        if wantsToPlay {
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

    func markInterrupted() {
        guard !stopped else { return }
        wasInterrupted = true
        wantsToPlay = false
        isPlaying = false
        isWaiting = false
        stateLabel = "Paused"
        bufferingRecoveryTask?.cancel()
        bufferingRecoveryTask = nil
        player.pause()
    }

    func reconnectAfterInterruption() async {
        guard wasInterrupted else { return }
        await recoverPlayback(reusePreparedStream: true)
    }

    func jump(by seconds: Double) {
        guard !stopped, durationSeconds > 0 else { return }
        let origin = pendingSeekSeconds ?? positionSeconds
        seek(to: origin + seconds)
    }

    private func scheduleBufferingRecovery() {
        bufferingRecoveryTask?.cancel()
        bufferingRecoveryTask = nil
        guard wantsToPlay, !isRecovering, !isSeeking, !wasInterrupted else { return }
        bufferingRecoveryTask = Task { @MainActor [weak self] in
            do {
                try await Task.sleep(for: .seconds(12))
            } catch {
                return
            }
            guard let self,
                  !self.stopped,
                  self.wantsToPlay,
                  !self.isSeeking,
                  self.player.timeControlStatus == .waitingToPlayAtSpecifiedRate else {
                return
            }
            await self.recoverPlayback(reusePreparedStream: false)
        }
    }

    private func recoverPlayback(reusePreparedStream: Bool) async {
        guard !stopped, !isRecovering else { return }
        isRecovering = true
        wasInterrupted = false
        bufferingRecoveryTask?.cancel()
        bufferingRecoveryTask = nil
        isWaiting = true
        stateLabel = "Reconnecting…"
        errorMessage = nil
        player.pause()

        let resumePosition = max(0, positionSeconds)
        var requestedStart = reusePreparedStream ? requestedStreamStartSeconds : resumePosition
        do {
            let refreshed: PreparedPlayback
            do {
                refreshed = try await api.prepareNativePlayback(
                    playback,
                    startSeconds: requestedStart
                )
            } catch {
                requestedStart = resumePosition
                let replacement = try await api.createPlayback(
                    for: movie,
                    startSeconds: requestedStart
                )
                refreshed = try await api.prepareNativePlaybackWithRetry(
                    replacement,
                    for: movie,
                    startSeconds: requestedStart
                )
            }
            guard !stopped else { return }

            playback = refreshed.playback
            requestedStreamStartSeconds = max(
                0,
                refreshed.hls.requestedStartSeconds ?? requestedStart
            )
            streamStartSeconds = max(0, refreshed.hls.startSeconds)
            updateSubtitleOptions(refreshed.hls.subtitles ?? [])
            if let duration = refreshed.hls.durationSeconds, duration > 0 {
                durationSeconds = duration
            }
            positionSeconds = resumePosition
            installItem(url: cacheBusted(refreshed.hls.playlistURL))

            let localPosition = max(0, resumePosition - streamStartSeconds)
            let time = CMTime(seconds: localPosition, preferredTimescale: 600)
            player.seek(to: time, toleranceBefore: .zero, toleranceAfter: .zero) { [weak self] finished in
                Task { @MainActor in
                    guard let self, !self.stopped else { return }
                    self.isRecovering = false
                    self.isWaiting = false
                    if !finished {
                        self.wantsToPlay = false
                        self.isPlaying = false
                        self.stateLabel = "Paused"
                        return
                    }
                    if self.wantsToPlay {
                        self.play()
                    } else {
                        self.isPlaying = false
                        self.stateLabel = "Paused"
                    }
                }
            }
        } catch {
            guard !stopped else { return }
            isRecovering = false
            isWaiting = false
            isPlaying = false
            wantsToPlay = false
            stateLabel = "Unable to Reconnect"
            errorMessage = error.localizedDescription
        }
    }

    func selectSubtitle(_ track: HLSSubtitleTrack?) {
        selectedSubtitle = track
        let defaults = UserDefaults.standard
        defaults.set(track != nil, forKey: "filmstream.subtitles.enabled")
        defaults.set(track?.language, forKey: "filmstream.subtitles.language")
        defaults.set(track?.title, forKey: "filmstream.subtitles.title")
        restartSubtitleUpdates()
    }

    func stop() {
        guard !stopped else { return }
        stopped = true
        seekGeneration += 1
        seekTask?.cancel()
        seekTask = nil
        subtitleTask?.cancel()
        subtitleTask = nil
        bufferingRecoveryTask?.cancel()
        bufferingRecoveryTask = nil
        deactivateMediaSession()
        if let itemEndObserver {
            NotificationCenter.default.removeObserver(itemEndObserver)
            self.itemEndObserver = nil
        }
        player.pause()
        player.isMuted = true
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

    private func seek(to requestedSeconds: Double) {
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
            requestedStreamStartSeconds = max(
                0,
                prepared.hls.requestedStartSeconds ?? target
            )
            streamStartSeconds = max(0, prepared.hls.startSeconds)
            updateSubtitleOptions(prepared.hls.subtitles ?? [])
            if let duration = prepared.hls.durationSeconds, duration > 0 {
                durationSeconds = duration
            }
            positionSeconds = target
            installItem(url: cacheBusted(prepared.hls.playlistURL))
            let localPosition = max(0, target - streamStartSeconds)
            let time = CMTime(seconds: localPosition, preferredTimescale: 600)
            player.seek(to: time, toleranceBefore: .zero, toleranceAfter: .zero) { [weak self] finished in
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

    private func restartSubtitleUpdates() {
        subtitleTask?.cancel()
        subtitleTask = nil
        subtitleCues = []
        activeSubtitleText = nil
        guard let track = selectedSubtitle, !stopped else { return }

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
                    // Subtitle polling is best-effort while the growing WebVTT file is written.
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
        let defaults = UserDefaults.standard
        if defaults.object(forKey: "filmstream.subtitles.enabled") == nil {
            return tracks.first(where: { $0.isForced == true })
        }
        guard defaults.bool(forKey: "filmstream.subtitles.enabled") else { return nil }
        let language = defaults.string(forKey: "filmstream.subtitles.language")
        let title = defaults.string(forKey: "filmstream.subtitles.title")
        return tracks.first(where: { $0.language == language && $0.title == title })
            ?? tracks.first(where: { $0.language == language })
    }

    private func installItem(url: URL) {
        statusObservation?.invalidate()
        if let itemEndObserver {
            NotificationCenter.default.removeObserver(itemEndObserver)
            self.itemEndObserver = nil
        }
        didReachEnd = false
        let item = AVPlayerItem(url: url)
        item.preferredForwardBufferDuration = 30
        statusObservation = item.observe(\.status, options: [.initial, .new]) { [weak self] item, _ in
            Task { @MainActor in
                guard let self, !self.stopped else { return }
                if item.status == .failed {
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
