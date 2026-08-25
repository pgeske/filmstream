import FilmstreamCore
import SwiftUI

struct IOSShowDetailView: View {
    @Environment(IOSAppModel.self) private var model
    let show: Movie

    @State private var details: SeriesDetails?
    @State private var playbackSelection: EpisodePlaybackSelection?
    @State private var activePlayback: IOSEpisodePlaybackSession?
    @State private var isLoading = false
    @State private var isPreparing = false
    @State private var preparationStage: PlaybackPreparationStage?
    @State private var isRemoving = false
    @State private var errorMessage: String?

    private var history: WatchHistoryEntry? { model.history(for: show) }
    private var ratings: MovieRatings? { model.ratings(for: show) }

    private var metadataSummary: String {
        let currentShow = details?.show ?? show
        var values: [String] = []
        if let genre = currentShow.primaryGenre {
            values.append(genre)
        }
        if let year = currentShow.year {
            values.append(String(year))
        }
        if let seasonCount = currentShow.seasonCountLabel {
            values.append(seasonCount)
        }
        return values.isEmpty ? "Show" : values.joined(separator: " • ")
    }

    var body: some View {
        GeometryReader { geometry in
            ZStack {
                MobileTeaBackground()

                ScrollView {
                    VStack(alignment: .leading, spacing: 0) {
                        backdrop(width: geometry.size.width)
                        content
                    }
                    .padding(.bottom, 36)
                }
                .frame(width: geometry.size.width, height: geometry.size.height)
                .ignoresSafeArea(edges: .top)
            }
        }
        .navigationTitle("")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
        .toolbarBackground(.hidden, for: .navigationBar)
        .toolbarColorScheme(.dark, for: .navigationBar)
        .task(id: show.id) {
            await loadShow()
        }
        .fullScreenCover(
            item: $activePlayback,
            onDismiss: {
                Task {
                    await model.loadContinueWatching()
                    await refreshPlaybackSelection()
                }
            }
        ) { session in
            IOSPlayerView(
                movie: session.movie,
                prepared: session.prepared,
                api: model.api,
                nextEpisode: session.nextEpisode,
                onPlayNext: { episode in
                    try await advancePlayback(to: episode)
                }
            )
            .id(session.id)
        }
    }

    private func backdrop(width: CGFloat) -> some View {
        MobileBackdropImage(movie: details?.show ?? show)
            .frame(width: width, height: 360)
            .clipped()
            .overlay {
                LinearGradient(
                    stops: [
                        .init(color: .clear, location: 0.2),
                        .init(color: Color.mobileTeaBackground.opacity(0.52), location: 0.64),
                        .init(color: Color.mobileTeaBackground, location: 1),
                    ],
                    startPoint: .top,
                    endPoint: .bottom
                )
            }
            .overlay {
                LinearGradient(
                    colors: [.black.opacity(0.42), .clear],
                    startPoint: .top,
                    endPoint: .center
                )
            }
            .overlay {
                LinearGradient(
                    colors: [Color.mobileTeaBackground.opacity(0.34), .clear],
                    startPoint: .leading,
                    endPoint: .trailing
                )
            }
            .overlay(alignment: .bottomLeading) {
                Text((details?.show ?? show).title)
                    .font(.system(size: 32, weight: .black, design: .rounded))
                    .foregroundStyle(Color.mobileTeaCream)
                    .lineLimit(3)
                    .padding(.horizontal, 18)
                    .padding(.bottom, 22)
            }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text(metadataSummary)
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(Color.mobileTeaMuted)

            HStack(spacing: 10) {
                if let contentRating = ratings?.contentRating {
                    Text(contentRating)
                        .font(.subheadline.weight(.semibold))
                        .foregroundStyle(Color.mobileTeaMuted)
                }
                MobileRatingBadges(ratings: ratings)
            }

            if let selection = playbackSelection {
                Text("\(selection.isResume ? "Resume" : "Play") \(selection.episode.label)")
                    .font(.subheadline.weight(.bold))
                    .foregroundStyle(Color.mobileTeaAccentLight)
            }

            if let overview = (details?.show ?? show).overview, !overview.isEmpty {
                Text(overview)
                    .font(.body)
                    .foregroundStyle(Color.mobileTeaCream.opacity(0.88))
                    .lineSpacing(3)
            }

            if let errorMessage {
                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(Color.mobileTeaAmber)
                    .padding(13)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(
                        Color.mobileTeaPanel.opacity(0.72),
                        in: RoundedRectangle(cornerRadius: 14, style: .continuous)
                    )
            }

            actionButtons
        }
        .padding(.horizontal, 18)
    }

    private var actionButtons: some View {
        VStack(spacing: 10) {
            Button {
                Task { await preparePlayback(startSeconds: playbackSelection?.startSeconds ?? 0) }
            } label: {
                actionLabel(
                    title: primaryButtonTitle,
                    systemImage: "play.fill",
                    showsProgress: isLoading || isPreparing,
                    progressTint: Color.mobileTeaBackground
                )
            }
            .buttonStyle(MobileDetailButtonStyle(kind: .prominent))
            .disabled(isLoading || isPreparing || playbackSelection == nil)

            if playbackSelection != nil {
                Button {
                    Task { await preparePlayback(startSeconds: 0) }
                } label: {
                    actionLabel(title: "Play from Beginning", systemImage: "arrow.counterclockwise")
                }
                .buttonStyle(MobileDetailButtonStyle(kind: .standard))
                .disabled(isPreparing || isRemoving)
            }

            if let details {
                NavigationLink {
                    IOSShowEpisodesView(details: details)
                } label: {
                    actionLabel(title: "Episodes & More", systemImage: "list.bullet")
                }
                .buttonStyle(MobileDetailButtonStyle(kind: .standard))
                .disabled(isPreparing || isRemoving)
            }

            if history != nil {
                Button(role: .destructive) {
                    Task { await removeFromContinueWatching() }
                } label: {
                    actionLabel(
                        title: isRemoving ? "Removing…" : "Remove from Continue Watching",
                        systemImage: "xmark",
                        showsProgress: isRemoving
                    )
                }
                .buttonStyle(MobileDetailButtonStyle(kind: .destructive))
                .disabled(isPreparing || isRemoving)
            }
        }
        .padding(.top, 2)
    }

    private var primaryButtonTitle: String {
        switch preparationStage {
        case .findingRelease:
            return "Finding a Release…"
        case .bufferingVideo:
            return "Buffering Video…"
        case nil:
            guard let playbackSelection else { return "Finding Your Episode…" }
            return "\(playbackSelection.isResume ? "Resume" : "Play") \(playbackSelection.episode.label)"
        }
    }

    private func actionLabel(
        title: String,
        systemImage: String,
        showsProgress: Bool = false,
        progressTint: Color = .mobileTeaCream
    ) -> some View {
        HStack(spacing: 11) {
            if showsProgress {
                ProgressView()
                    .controlSize(.small)
                    .tint(progressTint)
            } else {
                Image(systemName: systemImage)
                    .frame(width: 20)
            }
            Text(title)
                .lineLimit(1)
            Spacer()
        }
    }

    private func loadShow() async {
        isLoading = true
        defer { isLoading = false }
        do {
            Task { await model.loadRatings(for: show) }
            let loadedDetails = try await model.api.seriesDetails(for: show.id)
            details = loadedDetails
            playbackSelection = try await model.api.playbackSelection(
                for: loadedDetails,
                history: model.watchHistory
            )
            if let playbackSelection {
                await model.prewarmPlayback(
                    for: playbackSelection.episode.playbackMovie(in: loadedDetails.show),
                    startSeconds: playbackSelection.startSeconds
                )
            }
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    private func refreshPlaybackSelection() async {
        guard let details else { return }
        do {
            playbackSelection = try await model.api.playbackSelection(
                for: details,
                history: model.watchHistory
            )
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    private func preparePlayback(startSeconds: Double) async {
        guard let details, let playbackSelection else { return }
        isPreparing = true
        defer {
            isPreparing = false
            preparationStage = nil
        }
        let movie = playbackSelection.episode.playbackMovie(in: details.show)
        do {
            let nextEpisodeTask = Task {
                try? await model.api.nextEpisode(after: playbackSelection.episode, in: details)
            }
            let prepared = try await model.preparePlayback(
                for: movie,
                startSeconds: startSeconds,
                onStage: { preparationStage = $0 }
            )
            let nextEpisode = await nextEpisodeTask.value
            activePlayback = IOSEpisodePlaybackSession(
                movie: movie,
                prepared: prepared,
                nextEpisode: nextEpisode
            )
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    private func advancePlayback(to episode: Episode) async throws {
        guard let details else { return }
        let movie = episode.playbackMovie(in: details.show)
        let nextEpisodeTask = Task {
            try? await model.api.nextEpisode(after: episode, in: details)
        }
        let prepared = try await model.preparePlayback(
            for: movie,
            startSeconds: 0,
            onStage: { _ in }
        )
        let nextEpisode = await nextEpisodeTask.value
        activePlayback = IOSEpisodePlaybackSession(
            movie: movie,
            prepared: prepared,
            nextEpisode: nextEpisode
        )
    }

    private func removeFromContinueWatching() async {
        guard let history else { return }
        isRemoving = true
        defer { isRemoving = false }
        do {
            try await model.removeFromContinueWatching(history)
            await refreshPlaybackSelection()
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

private struct IOSEpisodePlaybackSession: Identifiable {
    let movie: Movie
    let prepared: PreparedPlayback
    let nextEpisode: Episode?

    var id: String { prepared.id }
}
