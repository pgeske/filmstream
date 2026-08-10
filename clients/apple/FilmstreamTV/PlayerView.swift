import AVKit
import Combine
import FilmstreamCore
import SwiftUI

struct PlayerView: View {
    let movie: Movie
    let prepared: PreparedPlayback
    let api: FilmstreamAPI

    @StateObject private var controller: NativePlaybackController
    @State private var didClose = false

    init(movie: Movie, prepared: PreparedPlayback, api: FilmstreamAPI) {
        self.movie = movie
        self.prepared = prepared
        self.api = api
        _controller = StateObject(
            wrappedValue: NativePlaybackController(
                url: prepared.hls.playlistURL,
                startOffset: prepared.hls.startSeconds,
                sourceDuration: prepared.hls.durationSeconds ?? 0
            )
        )
    }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            NativePlayerSurface(player: controller.player)
                .ignoresSafeArea()

            if controller.isWaiting {
                VStack(spacing: 18) {
                    ProgressView()
                        .controlSize(.large)
                    Text(controller.stateLabel)
                        .font(.headline)
                    Text(prepared.playback.fileName)
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                .padding(30)
                .background(.black.opacity(0.72), in: RoundedRectangle(cornerRadius: 20))
                .allowsHitTesting(false)
            }
        }
        .onAppear {
            controller.play()
        }
        .onDisappear {
            closePlayback()
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

    private func closePlayback() {
        guard !didClose else { return }
        didClose = true
        let position = controller.positionSeconds
        let duration = controller.durationSeconds
        controller.stop()
        Task {
            if position > 0 {
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
        guard controller.positionSeconds > 0 else { return }
        _ = try? await api.updateProgress(
            for: movie,
            positionSeconds: controller.positionSeconds,
            durationSeconds: controller.durationSeconds
        )
    }
}

private struct NativePlayerSurface: UIViewControllerRepresentable {
    let player: AVPlayer

    func makeUIViewController(context: Context) -> AVPlayerViewController {
        let controller = AVPlayerViewController()
        controller.player = player
        controller.showsPlaybackControls = true
        return controller
    }

    func updateUIViewController(_ controller: AVPlayerViewController, context: Context) {
        if controller.player !== player {
            controller.player = player
        }
    }

    static func dismantleUIViewController(_ controller: AVPlayerViewController, coordinator: Void) {
        controller.player = nil
    }
}

@MainActor
private final class NativePlaybackController: ObservableObject {
    let player: AVPlayer

    @Published private(set) var positionSeconds: Double
    @Published private(set) var durationSeconds: Double
    @Published private(set) var isWaiting = true
    @Published private(set) var stateLabel = "Preparing Stream…"

    private let startOffset: Double
    private var timeObserver: Any?
    private var statusObservation: NSKeyValueObservation?
    private var playbackObservation: NSKeyValueObservation?
    private var stopped = false

    init(url: URL, startOffset: Double, sourceDuration: Double) {
        self.startOffset = max(0, startOffset)
        positionSeconds = max(0, startOffset)
        durationSeconds = max(0, sourceDuration)

        let item = AVPlayerItem(url: url)
        item.preferredForwardBufferDuration = 20
        player = AVPlayer(playerItem: item)
        player.automaticallyWaitsToMinimizeStalling = true
        player.actionAtItemEnd = .pause

        timeObserver = player.addPeriodicTimeObserver(
            forInterval: CMTime(seconds: 1, preferredTimescale: 600),
            queue: .main
        ) { [weak self] time in
            Task { @MainActor in
                guard let self else { return }
                let current = time.seconds
                if current.isFinite, current >= 0 {
                    self.positionSeconds = self.startOffset + current
                }
                let itemDuration = self.player.currentItem?.duration.seconds ?? 0
                if self.durationSeconds == 0, itemDuration.isFinite, itemDuration > 0 {
                    self.durationSeconds = self.startOffset + itemDuration
                }
            }
        }
        statusObservation = item.observe(\.status, options: [.initial, .new]) { [weak self] item, _ in
            Task { @MainActor in
                guard let self else { return }
                if item.status == .failed {
                    self.isWaiting = false
                    self.stateLabel = item.error?.localizedDescription ?? "Unable to Play Stream"
                }
            }
        }
        playbackObservation = player.observe(\.timeControlStatus, options: [.initial, .new]) { [weak self] player, _ in
            Task { @MainActor in
                guard let self else { return }
                switch player.timeControlStatus {
                case .playing:
                    self.isWaiting = false
                    self.stateLabel = "Playing"
                case .waitingToPlayAtSpecifiedRate:
                    self.isWaiting = true
                    self.stateLabel = "Buffering…"
                case .paused:
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
        player.play()
    }

    func stop() {
        guard !stopped else { return }
        stopped = true
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
}
