import FilmstreamCore
import SwiftUI

struct MacShowDetailView: View {
    @Environment(MacAppModel.self) private var model
    let show: Movie

    @State private var details: SeriesDetails?
    @State private var playbackSelection: EpisodePlaybackSelection?
    @State private var isLoading = false
    @State private var preparation = PlaybackPreparation()
    private var isPreparing: Bool { preparation.isPreparing }
    private var preparationStage: PlaybackPreparationStage? { preparation.stage }
    @State private var isRemoving = false
    @State private var errorMessage: String?

    private var history: WatchHistoryEntry? { model.history(for: show) }
    private var ratings: MovieRatings? { model.ratings(for: show) }

    var body: some View {
        GeometryReader { geometry in
            ZStack(alignment: .leading) {
                MacTeaBackground()

                MacBackdropImage(movie: details?.show ?? show)
                    .frame(width: geometry.size.width, height: geometry.size.height)
                    .clipped()

                LinearGradient(
                    stops: [
                        .init(color: Color.macTeaBackground.opacity(0.99), location: 0),
                        .init(color: Color.macTeaBackground.opacity(0.87), location: 0.4),
                        .init(color: Color.macTeaBackground.opacity(0.2), location: 0.8),
                        .init(color: .clear, location: 1),
                    ],
                    startPoint: .leading,
                    endPoint: .trailing
                )

                LinearGradient(
                    stops: [
                        .init(color: .clear, location: 0.5),
                        .init(color: Color.macTeaBackground.opacity(0.7), location: 0.86),
                        .init(color: Color.macTeaBackground, location: 1),
                    ],
                    startPoint: .top,
                    endPoint: .bottom
                )

                ScrollView {
                    VStack(alignment: .leading, spacing: 17) {
                        Spacer(minLength: 70)

                        MacTeaStreamMark(size: 40)

                        Text((details?.show ?? show).title)
                            .font(.system(size: 46, weight: .black, design: .rounded))
                            .foregroundStyle(Color.macTeaCream)
                            .lineLimit(2)
                            .frame(maxWidth: 720, alignment: .leading)

                        metadataLine

                        if let overview = (details?.show ?? show).overview, !overview.isEmpty {
                            Text(overview)
                                .font(.body)
                                .foregroundStyle(Color.macTeaCream.opacity(0.88))
                                .lineSpacing(4)
                                .frame(maxWidth: 680, alignment: .leading)
                        }

                        if let errorMessage = preparation.errorMessage ?? errorMessage {
                            Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                                .foregroundStyle(Color.macTeaAmber)
                                .font(.headline)
                                .frame(maxWidth: 680, alignment: .leading)
                        }

                        actionButtons
                    }
                    .padding(.horizontal, 46)
                    .padding(.vertical, 36)
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                .frame(width: geometry.size.width, height: geometry.size.height)
            }
        }
        .background(Color.macTeaBackground)
        .navigationTitle(show.title)
        .task(id: show.id) {
            await loadShow()
        }
        .onDisappear { preparation.cancel() }
        .onChange(of: model.watchHistory) {
            Task { await refreshPlaybackSelection() }
        }
    }

    private var metadataLine: some View {
        HStack(spacing: 11) {
            Text((details?.show ?? show).catalogMetadata)
            if let contentRating = ratings?.contentRating {
                Text("•")
                Text(contentRating)
            }
            MacMovieRatingBadges(ratings: ratings)
            if let selection = playbackSelection {
                Text("•")
                Text(selection.episode.label)
                    .foregroundStyle(Color.macTeaAccentLight)
            }
        }
        .font(.headline)
        .foregroundStyle(Color.macTeaMuted)
    }

    private var actionButtons: some View {
        VStack(alignment: .leading, spacing: 10) {
            Button {
                preparePlayback(startSeconds: playbackSelection?.startSeconds ?? 0)
            } label: {
                actionLabel(
                    title: primaryButtonTitle,
                    systemImage: "play.fill",
                    showsProgress: isLoading || isPreparing
                )
            }
            .buttonStyle(MacDetailButtonStyle(kind: .prominent))
            .disabled(isLoading || isPreparing || playbackSelection == nil)
            .keyboardShortcut(.defaultAction)

            if playbackSelection != nil {
                Button {
                    preparePlayback(startSeconds: 0)
                } label: {
                    actionLabel(title: "Play from Beginning", systemImage: "arrow.counterclockwise")
                }
                .buttonStyle(MacDetailButtonStyle(kind: .standard))
                .disabled(isPreparing || isRemoving)
            }

            if let details {
                NavigationLink {
                    MacShowEpisodesView(details: details)
                } label: {
                    actionLabel(title: "Episodes & More", systemImage: "list.bullet")
                }
                .buttonStyle(MacDetailButtonStyle(kind: .standard))
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
                .buttonStyle(MacDetailButtonStyle(kind: .destructive))
                .disabled(isPreparing || isRemoving)
            }
        }
        .padding(.top, 3)
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
        showsProgress: Bool = false
    ) -> some View {
        HStack(spacing: 11) {
            if showsProgress {
                ZStack {
                    Circle()
                        .fill(Color.macTeaBackground.opacity(0.08))
                        .frame(width: 26, height: 26)
                    ProgressView()
                        .controlSize(.small)
                        .tint(Color.macTeaBackground)
                }
                .frame(width: 28, height: 28)
            } else {
                Image(systemName: systemImage)
                    .frame(width: 20)
            }
            Text(title)
                .lineLimit(1)
            Spacer()
        }
        .frame(width: 420)
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

    private func preparePlayback(startSeconds: Double) {
        guard let details, let playbackSelection else { return }
        errorMessage = nil
        let movie = playbackSelection.episode.playbackMovie(in: details.show)
        preparation.start(
            api: model.api,
            movie: movie,
            startSeconds: startSeconds,
            nextEpisode: { try? await model.api.nextEpisode(after: playbackSelection.episode, in: details) }
        ) { prepared, nextEpisode in
            model.presentPlayback(
                movie: movie, prepared: prepared, details: details, nextEpisode: nextEpisode
            )
        }
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
