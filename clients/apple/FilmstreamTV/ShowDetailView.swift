import FilmstreamCore
import SwiftUI

struct ShowDetailView: View {
    @Environment(AppModel.self) private var model
    let show: Movie

    @State private var details: SeriesDetails?
    @State private var playbackSelection: EpisodePlaybackSelection?
    @State private var activePlayback: TVEpisodePlaybackSession?
    @State private var showsEpisodes = false
    @State private var isLoading = false
    @State private var isPreparing = false
    @State private var preparationStage: PlaybackPreparationStage?
    @State private var isRemoving = false
    @State private var errorMessage: String?
    @FocusState private var focusedAction: ShowDetailAction?

    private enum ShowDetailAction: Hashable {
        case play
        case startOver
        case episodes
        case remove
    }

    private var history: WatchHistoryEntry? {
        model.history(for: show)
    }

    private var ratings: MovieRatings? {
        model.ratings(for: show)
    }

    var body: some View {
        ZStack(alignment: .leading) {
            BackdropImage(movie: details?.show ?? show)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .clipped()
                .ignoresSafeArea()

            LinearGradient(
                stops: [
                    .init(color: Color.teaBackground.opacity(0.99), location: 0),
                    .init(color: Color.teaBackground.opacity(0.9), location: 0.34),
                    .init(color: Color.teaBackground.opacity(0.24), location: 0.74),
                    .init(color: .clear, location: 1),
                ],
                startPoint: .leading,
                endPoint: .trailing
            )
            .ignoresSafeArea()

            LinearGradient(
                stops: [
                    .init(color: .clear, location: 0.48),
                    .init(color: Color.teaBackground.opacity(0.64), location: 0.82),
                    .init(color: Color.teaBackground, location: 1),
                ],
                startPoint: .top,
                endPoint: .bottom
            )
            .ignoresSafeArea()

            VStack(alignment: .leading, spacing: 20) {
                Spacer(minLength: 30)

                Text((details?.show ?? show).title)
                    .font(.system(size: 62, weight: .black, design: .rounded))
                    .foregroundStyle(Color.teaCream)
                    .lineLimit(2)
                    .frame(maxWidth: 780, alignment: .leading)

                metadataLine

                if let overview = (details?.show ?? show).overview, !overview.isEmpty {
                    Text(overview)
                        .font(.system(size: 26, weight: .regular, design: .rounded))
                        .foregroundStyle(Color.teaCream.opacity(0.86))
                        .lineSpacing(3)
                        .lineLimit(5)
                        .frame(maxWidth: 800, alignment: .leading)
                }

                if let errorMessage {
                    Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(Color.teaAmber)
                        .font(.headline)
                        .frame(maxWidth: 720, alignment: .leading)
                }

                actionButtons
            }
            .padding(.leading, 82)
            .padding(.trailing, 60)
            .padding(.vertical, 54)
        }
        .background(Color.teaBackground)
        .task(id: show.id) {
            focusedAction = .play
            await loadShow()
        }
        .navigationDestination(isPresented: $showsEpisodes) {
            if let details {
                ShowEpisodesView(details: details)
            }
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
            PlayerView(movie: session.movie, prepared: session.prepared, api: model.api)
        }
    }

    private var metadataLine: some View {
        HStack(spacing: 15) {
            Text((details?.show ?? show).catalogMetadata)
            if let contentRating = ratings?.contentRating {
                Text("•")
                Text(contentRating)
            }
            MovieRatingBadges(ratings: ratings)
            if let selection = playbackSelection {
                Text("•")
                Text(selection.episode.label)
                    .foregroundStyle(Color.teaAccentLight)
            }
        }
        .font(.headline)
        .foregroundStyle(Color.teaMuted)
    }

    private var actionButtons: some View {
        VStack(alignment: .leading, spacing: 11) {
            Button {
                Task { await preparePlayback(startSeconds: playbackSelection?.startSeconds ?? 0) }
            } label: {
                actionLabel(
                    title: primaryButtonTitle,
                    systemImage: "play.fill",
                    showsProgress: isPreparing || isLoading,
                    progressTint: Color.teaBackground
                )
            }
            .buttonStyle(TeaDetailActionButtonStyle(kind: .prominent))
            .focusEffectDisabled()
            .focused($focusedAction, equals: .play)
            .disabled(isLoading || isPreparing || playbackSelection == nil)

            if playbackSelection != nil {
                Button {
                    Task { await preparePlayback(startSeconds: 0) }
                } label: {
                    actionLabel(title: "Play from Beginning", systemImage: "arrow.counterclockwise")
                }
                .buttonStyle(TeaDetailActionButtonStyle(kind: .standard))
                .focusEffectDisabled()
                .focused($focusedAction, equals: .startOver)
                .disabled(isPreparing || isRemoving)
            }

            if details != nil {
                Button {
                    showsEpisodes = true
                } label: {
                    actionLabel(title: "Episodes & More", systemImage: "list.bullet")
                }
                .buttonStyle(TeaDetailActionButtonStyle(kind: .standard))
                .focusEffectDisabled()
                .focused($focusedAction, equals: .episodes)
                .disabled(isPreparing || isRemoving)
            }

            if history != nil {
                Button {
                    Task { await removeFromContinueWatching() }
                } label: {
                    actionLabel(
                        title: isRemoving ? "Removing…" : "Remove from Continue Watching",
                        systemImage: "xmark",
                        showsProgress: isRemoving
                    )
                }
                .buttonStyle(TeaDetailActionButtonStyle(kind: .destructive))
                .focusEffectDisabled()
                .focused($focusedAction, equals: .remove)
                .disabled(isPreparing || isRemoving)
            }
        }
        .padding(.top, 4)
    }

    private var primaryButtonTitle: String {
        switch preparationStage {
        case .findingRelease:
            return "Finding a Release…"
        case .bufferingVideo:
            return "Buffering Video…"
        case nil:
            guard let playbackSelection else { return "Finding Your Episode…" }
            let action = playbackSelection.isResume ? "Resume" : "Play"
            return "\(action) \(playbackSelection.episode.label)"
        }
    }

    private func actionLabel(
        title: String,
        systemImage: String,
        showsProgress: Bool = false,
        progressTint: Color = .teaCream
    ) -> some View {
        HStack(spacing: 14) {
            if showsProgress {
                ProgressView()
                    .tint(progressTint)
            } else {
                Image(systemName: systemImage)
                    .frame(width: 26)
            }
            Text(title)
                .lineLimit(1)
            Spacer()
        }
        .font(.headline.weight(.bold))
        .frame(width: 740)
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
            let prepared = try await model.preparePlayback(
                for: movie,
                startSeconds: startSeconds,
                onStage: { preparationStage = $0 }
            )
            activePlayback = TVEpisodePlaybackSession(movie: movie, prepared: prepared)
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    private func removeFromContinueWatching() async {
        guard let history else { return }
        isRemoving = true
        defer { isRemoving = false }
        do {
            try await model.removeFromContinueWatching(history)
            errorMessage = nil
            focusedAction = .play
            await refreshPlaybackSelection()
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

private struct TVEpisodePlaybackSession: Identifiable {
    let movie: Movie
    let prepared: PreparedPlayback

    var id: String { prepared.id }
}
