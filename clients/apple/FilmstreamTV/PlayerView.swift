import AVFoundation
import Combine
import FilmstreamCore
import SwiftUI
import UIKit

struct PlayerView: View {
    @Environment(\.dismiss) private var dismiss

    let movie: Movie
    let prepared: PreparedPlayback
    let api: FilmstreamAPI

    @StateObject private var controller: NativePlaybackController
    @State private var didClose = false
    @FocusState private var receivesRemoteCommands: Bool

    init(movie: Movie, prepared: PreparedPlayback, api: FilmstreamAPI) {
        self.movie = movie
        self.prepared = prepared
        self.api = api
        _controller = StateObject(
            wrappedValue: NativePlaybackController(prepared: prepared, api: api)
        )
    }

    var body: some View {
        ZStack(alignment: .bottom) {
            Color.black.ignoresSafeArea()
            NativePlayerSurface(player: controller.player)
                .ignoresSafeArea()

            LinearGradient(
                colors: [.clear, .black.opacity(0.86)],
                startPoint: .top,
                endPoint: .bottom
            )
            .frame(height: 270)
            .allowsHitTesting(false)

            controls
                .padding(.horizontal, 68)
                .padding(.bottom, 42)

            if controller.isWaiting || controller.errorMessage != nil {
                VStack(spacing: 18) {
                    if controller.errorMessage == nil {
                        ProgressView()
                            .controlSize(.large)
                    } else {
                        Image(systemName: "exclamationmark.triangle.fill")
                            .font(.largeTitle)
                            .foregroundStyle(.orange)
                    }
                    Text(controller.stateLabel)
                        .font(.headline)
                    Text(controller.errorMessage ?? prepared.playback.fileName)
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .lineLimit(2)
                }
                .padding(30)
                .background(.black.opacity(0.76), in: RoundedRectangle(cornerRadius: 20))
                .allowsHitTesting(false)
            }
        }
        .focusable()
        .focused($receivesRemoteCommands)
        .onAppear {
            receivesRemoteCommands = true
            controller.play()
        }
        .onDisappear {
            closePlayback()
        }
        .onTapGesture {
            controller.togglePlayback()
        }
        .onPlayPauseCommand {
            controller.togglePlayback()
        }
        .onMoveCommand { direction in
            switch direction {
            case .left:
                controller.jump(by: -30)
            case .right:
                controller.jump(by: 30)
            default:
                break
            }
        }
        .onExitCommand {
            closePlayback()
            dismiss()
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

    private var controls: some View {
        VStack(alignment: .leading, spacing: 15) {
            HStack {
                VStack(alignment: .leading, spacing: 5) {
                    Text(movie.title)
                        .font(.title2.weight(.bold))
                    Text(prepared.playback.fileName)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                Spacer()
                Label(
                    controller.stateLabel,
                    systemImage: controller.isPlaying ? "pause.fill" : "play.fill"
                )
                .foregroundStyle(.secondary)
            }

            ProgressView(
                value: controller.positionSeconds,
                total: max(controller.durationSeconds, 1)
            )
            .tint(Color.filmstreamAccent)

            HStack {
                Text(formatTime(controller.positionSeconds))
                Spacer()
                Text("Center or Play/Pause toggles playback • Left/right seeks 30 seconds anywhere")
                    .foregroundStyle(.secondary)
                Spacer()
                Text(formatTime(controller.durationSeconds))
            }
            .font(.footnote.monospacedDigit())
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
    let player = AVPlayer()

    @Published private(set) var positionSeconds: Double
    @Published private(set) var durationSeconds: Double
    @Published private(set) var isPlaying = false
    @Published private(set) var isSeeking = false
    @Published private(set) var isWaiting = true
    @Published private(set) var stateLabel = "Preparing Stream…"
    @Published private(set) var errorMessage: String?

    private let api: FilmstreamAPI
    private let playback: Playback
    private var streamStartSeconds: Double
    private var timeObserver: Any?
    private var statusObservation: NSKeyValueObservation?
    private var playbackObservation: NSKeyValueObservation?
    private var seekTask: Task<Void, Never>?
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
        positionSeconds = max(0, prepared.hls.startSeconds)
        durationSeconds = max(0, prepared.hls.durationSeconds ?? 0)
        seekOriginSeconds = max(0, prepared.hls.startSeconds)

        player.automaticallyWaitsToMinimizeStalling = true
        player.actionAtItemEnd = .pause
        installItem(url: prepared.hls.playlistURL)

        timeObserver = player.addPeriodicTimeObserver(
            forInterval: CMTime(seconds: 1, preferredTimescale: 600),
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
                }
            }
        }

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

    func stop() {
        guard !stopped else { return }
        stopped = true
        seekGeneration += 1
        seekTask?.cancel()
        seekTask = nil
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
            streamStartSeconds = max(0, prepared.hls.startSeconds)
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

    private func installItem(url: URL) {
        statusObservation?.invalidate()
        let item = AVPlayerItem(url: url)
        item.preferredForwardBufferDuration = 20
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
