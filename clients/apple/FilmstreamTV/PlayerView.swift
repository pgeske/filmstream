import AVFoundation
import FilmstreamCore
import Foundation
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
    @State private var nextEpisodeTask: Task<Void, Never>?
    @State private var didSaveEndProgress = false
    @State private var isPlaybackChromeVisible = true
    @State private var isSubtitlePickerPresented = false
    @State private var isAudioPickerPresented = false
    @State private var isStartingNextEpisode = false
    @State private var nextEpisodeError: String?
    @State private var chromeAutoHideTask: Task<Void, Never>?
    @FocusState private var receivesRemoteCommands: Bool

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
            NativePlayerSurface(controller: controller)
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
                VStack(spacing: 20) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.largeTitle)
                        .foregroundStyle(Color.teaAmber)
                    Text(nextEpisodeError == nil ? "Unable to Play" : "Unable to Start Next Episode")
                        .font(.title2.weight(.bold))
                        .foregroundStyle(Color.teaCream)
                    Text(errorMessage)
                        .font(.title3)
                        .foregroundStyle(Color.teaCream)
                        .multilineTextAlignment(.center)
                        .lineLimit(4)
                    Text(nextEpisodeError == nil ? "Press Play to try again, or Back to close." : "Press Back to close.")
                        .font(.headline)
                        .foregroundStyle(Color.teaAccentLight)
                }
                .frame(maxWidth: 1_100)
                .padding(40)
                .background(.black.opacity(0.84), in: RoundedRectangle(cornerRadius: 24))
                .allowsHitTesting(false)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
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
                VStack(spacing: 22) {
                    PlaybackLoadingIndicator()
                    if let detail = controller.waitingDetail {
                        Text(detail)
                            .font(.title3.weight(.semibold))
                            .foregroundStyle(Color.teaCream)
                            .multilineTextAlignment(.center)
                            .lineLimit(2)
                            .padding(.horizontal, 30)
                            .padding(.vertical, 16)
                            .background(.black.opacity(0.76), in: RoundedRectangle(cornerRadius: 18))
                    }
                }
                .frame(maxWidth: 1_200)
                .allowsHitTesting(false)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
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

            if isAudioPickerPresented {
                AudioPicker(
                    tracks: controller.audioOptions,
                    selected: controller.selectedAudio,
                    onSelect: { track in
                        controller.selectAudio(track)
                        Task { @MainActor in
                            try? await Task.sleep(for: .milliseconds(100))
                            closeAudioPicker()
                        }
                    },
                    onDismiss: closeAudioPicker
                )
                .transition(.opacity.combined(with: .move(edge: .trailing)))
            }
        }
        .focusable(!isSubtitlePickerPresented && !isAudioPickerPresented)
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
                controller.resumeAfterInterruption()
            case .inactive, .background:
                // System overlays make the scene inactive: pause, keep the stream.
                controller.interrupt()
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
            guard !isSubtitlePickerPresented, !isAudioPickerPresented else { return }
            controller.togglePlayback()
            revealPlaybackChrome()
        }
        .onMoveCommand { direction in
            guard !isSubtitlePickerPresented, !isAudioPickerPresented else { return }
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
                guard !controller.audioOptions.isEmpty else {
                    revealPlaybackChrome()
                    return
                }
                revealPlaybackChrome(autoHide: false)
                receivesRemoteCommands = false
                withAnimation(.easeOut(duration: 0.2)) {
                    isAudioPickerPresented = true
                }
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
        .onExitCommand {
            if isSubtitlePickerPresented {
                closeSubtitlePicker()
            } else if isAudioPickerPresented {
                closeAudioPicker()
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
                if !isStartingNextEpisode {
                    await reportProgress()
                }
            }
        }
    }

    private var isPlaybackChromePresented: Bool {
        isPlaybackChromeVisible || isSubtitlePickerPresented || isAudioPickerPresented
    }

    private var controls: some View {
        VStack(alignment: .leading, spacing: 15) {
            HStack {
                Text(movie.title)
                    .font(.title2.weight(.bold))
                Spacer()
                VStack(alignment: .trailing, spacing: 9) {
                    Label(
                        controller.stateLabel,
                        systemImage: controller.isPlaying ? "pause.fill" : "play.fill"
                    )
                    .font(.headline)
                    Label(
                        controller.selectedSubtitle?.displayName
                            ?? (controller.subtitleOptions.isEmpty ? "No Subtitles" : "Subtitles Off"),
                        systemImage: "captions.bubble"
                    )
                    if controller.audioOptions.count > 1, let audio = controller.selectedAudio {
                        Label(audio.displayName, systemImage: "speaker.wave.2")
                    }
                }
                .font(.body)
                .foregroundStyle(Color.teaCream)
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
            .font(.headline.monospacedDigit())
            .foregroundStyle(Color.teaCream)
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
              !isAudioPickerPresented,
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
        controller.pause()
        revealPlaybackChrome(autoHide: false)

        nextEpisodeTask = Task { @MainActor in
            didSaveEndProgress = await reportProgress()
            do {
                try Task.checkCancellation()
                try await onPlayNext(nextEpisode)
            } catch {
                guard !Task.isCancelled, !didClose else { return }
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

    private func closeAudioPicker() {
        withAnimation(.easeOut(duration: 0.18)) {
            isAudioPickerPresented = false
        }
        Task { @MainActor in
            receivesRemoteCommands = true
            schedulePlaybackChromeAutoHide()
        }
    }

    private func closePlayback() {
        guard !didClose else { return }
        didClose = true
        nextEpisodeTask?.cancel()
        nextEpisodeTask = nil
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

private struct AudioPicker: View {
    let tracks: [HLSAudioTrack]
    let selected: HLSAudioTrack?
    let onSelect: (HLSAudioTrack) -> Void
    let onDismiss: () -> Void

    @FocusState private var focusedOption: Int?

    private var listHeight: CGFloat {
        min(CGFloat(tracks.count) * 76, 520)
    }

    var body: some View {
        ZStack(alignment: .trailing) {
            Color.black.opacity(0.34)
                .ignoresSafeArea()

            VStack(alignment: .leading, spacing: 20) {
                HStack(spacing: 14) {
                    Image(systemName: "speaker.wave.2.fill")
                        .foregroundStyle(Color.teaAccentLight)
                    Text("Audio")
                        .font(.system(size: 34, weight: .bold, design: .rounded))
                }

                Text("Choose a track")
                    .font(.headline)
                    .foregroundStyle(.white.opacity(0.55))

                ScrollView {
                    LazyVStack(spacing: 8) {
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
            focusedOption = selected?.index ?? tracks.first?.index
        }
    }

    private func displayName(for track: HLSAudioTrack) -> String {
        let name = track.displayName
        let matching = tracks.filter { $0.displayName == name }
        guard matching.count > 1,
              let position = matching.firstIndex(where: { $0.index == track.index }),
              position > 0 else {
            return name
        }
        return "\(name) \(position + 1)"
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
            .padding(.horizontal, 18)
            .frame(height: 60)
            .background(
                focusedOption == id ? Color.white.opacity(0.08) : .clear,
                in: RoundedRectangle(cornerRadius: 14, style: .continuous)
            )
        }
        .buttonStyle(SubtitleOptionButtonStyle())
        .focusEffectDisabled()
        .focused($focusedOption, equals: id)
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
    let controller: NativePlaybackController
    private var player: AVPlayer { controller.player }

    func makeUIView(context: Context) -> PlayerViewSurface {
        let view = PlayerViewSurface()
        view.player = player
        controller.attachVideoLayer(view.playerLayer)
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
