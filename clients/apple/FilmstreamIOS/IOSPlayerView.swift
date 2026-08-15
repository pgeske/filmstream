import AVFoundation
import Combine
import FilmstreamCore
import Foundation
import SwiftUI
import UIKit

struct IOSPlayerView: View {
    @Environment(\.dismiss) private var dismiss

    let movie: Movie
    let prepared: PreparedPlayback
    let api: FilmstreamAPI

    @StateObject private var controller: IOSPlaybackController
    @State private var didClose = false
    @State private var isChromeVisible = true
    @State private var scrubPosition: Double?
    @State private var autoHideTask: Task<Void, Never>?

    init(movie: Movie, prepared: PreparedPlayback, api: FilmstreamAPI) {
        self.movie = movie
        self.prepared = prepared
        self.api = api
        _controller = StateObject(
            wrappedValue: IOSPlaybackController(prepared: prepared, api: api)
        )
    }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            IOSPlayerSurface(player: controller.player)
                .ignoresSafeArea()
                .contentShape(Rectangle())
                .onTapGesture {
                    revealChrome()
                }

            if let subtitle = controller.activeSubtitleText {
                VStack {
                    Spacer()
                    Text(subtitle)
                        .font(.system(size: 22, weight: .semibold, design: .rounded))
                        .foregroundStyle(.white)
                        .multilineTextAlignment(.center)
                        .lineSpacing(3)
                        .padding(.horizontal, 24)
                        .padding(.bottom, isChromeVisible ? 180 : 28)
                        .shadow(color: .black, radius: 3, x: 1, y: 1)
                        .shadow(color: .black.opacity(0.9), radius: 7)
                        .animation(.easeOut(duration: 0.2), value: isChromeVisible)
                }
                .allowsHitTesting(false)
            }

            if isChromeVisible {
                chrome
                    .transition(.opacity)
            }

            if let errorMessage = controller.errorMessage {
                VStack(spacing: 12) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.largeTitle)
                        .foregroundStyle(Color.mobileTeaAmber)
                    Text("Unable to Play")
                        .font(.headline)
                    Text(errorMessage)
                        .font(.footnote)
                        .foregroundStyle(Color.mobileTeaMuted)
                        .multilineTextAlignment(.center)
                }
                .padding(24)
                .frame(maxWidth: 330)
                .background(.black.opacity(0.82), in: RoundedRectangle(cornerRadius: 18))
            } else if controller.isWaiting {
                ProgressView()
                    .controlSize(.large)
                    .tint(Color.mobileTeaAccent)
                    .padding(22)
                    .background(.black.opacity(0.58), in: Circle())
                    .allowsHitTesting(false)
            }
        }
        .statusBarHidden(true)
        .persistentSystemOverlays(.hidden)
        .onAppear {
            UIApplication.shared.isIdleTimerDisabled = true
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
                    .background(.black.opacity(0.52), in: Circle())
            }
            .accessibilityLabel("Close player")

            Text(movie.title)
                .font(.headline)
                .lineLimit(1)

            Spacer()

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
                    .background(.black.opacity(0.52), in: Circle())
            }
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
                playerButton(
                    systemImage: controller.isPlaying ? "pause.fill" : "play.fill",
                    label: controller.isPlaying ? "Pause" : "Play",
                    prominent: true
                ) {
                    controller.togglePlayback()
                    revealChrome()
                }
                playerButton(systemImage: "goforward.30", label: "Forward 30 seconds") {
                    controller.jump(by: 30)
                    revealChrome()
                }
                Spacer()
            }
        }
        .foregroundStyle(.white)
        .padding(.horizontal, 20)
        .padding(.top, 42)
        .padding(.bottom, 16)
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
                .foregroundStyle(prominent ? Color.mobileTeaBackground : .white)
        }
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
        }
    }

    private func synchronizeChrome() {
        if controller.isSeeking || (!controller.isPlaying && !controller.isWaiting) {
            revealChrome(autoHide: false)
        } else if isChromeVisible {
            scheduleAutoHide()
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
            try? await api.stopNativePlayback(prepared.playback.id)
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

private struct IOSPlayerSurface: UIViewRepresentable {
    let player: AVPlayer

    func makeUIView(context: Context) -> IOSPlayerSurfaceView {
        let view = IOSPlayerSurfaceView()
        view.player = player
        return view
    }

    func updateUIView(_ view: IOSPlayerSurfaceView, context: Context) {
        if view.player !== player {
            view.player = player
        }
    }

    static func dismantleUIView(_ view: IOSPlayerSurfaceView, coordinator: Void) {
        view.player = nil
    }
}

private final class IOSPlayerSurfaceView: UIView {
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
    @Published private(set) var stateLabel = "Preparing Stream…"
    @Published private(set) var errorMessage: String?
    @Published private(set) var subtitleOptions: [HLSSubtitleTrack]
    @Published private(set) var selectedSubtitle: HLSSubtitleTrack?
    @Published private(set) var activeSubtitleText: String?

    private let api: FilmstreamAPI
    private let playback: Playback
    private var streamStartSeconds: Double
    private var timeObserver: Any?
    private var statusObservation: NSKeyValueObservation?
    private var playbackObservation: NSKeyValueObservation?
    private var seekTask: Task<Void, Never>?
    private var subtitleTask: Task<Void, Never>?
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

        try? AVAudioSession.sharedInstance().setCategory(.playback, mode: .moviePlayback)
        try? AVAudioSession.sharedInstance().setActive(true)

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
                    self.isPlaying = true
                    self.isWaiting = false
                    self.stateLabel = "Playing"
                case .waitingToPlayAtSpecifiedRate:
                    self.isPlaying = self.wantsToPlay
                    self.isWaiting = true
                    self.stateLabel = "Buffering…"
                case .paused:
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
        wantsToPlay = true
        isPlaying = true
        player.play()
    }

    func togglePlayback() {
        guard !stopped else { return }
        if isSeeking {
            resumeAfterSeek.toggle()
            isPlaying = resumeAfterSeek
            return
        }
        if wantsToPlay {
            wantsToPlay = false
            isPlaying = false
            player.pause()
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
