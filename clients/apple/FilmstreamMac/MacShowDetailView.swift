import FilmstreamCore
import SwiftUI

struct MacShowDetailView: View {
    @Environment(MacAppModel.self) private var model
    let show: Movie

    @State private var details: SeriesDetails?
    @State private var playbackSelection: EpisodePlaybackSelection?
    @State private var isLoading = false
    @State private var isPreparing = false
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

                        if let errorMessage {
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
            MacMovieRatingBadges(ratings: ratings, tmdbRating: (details?.show ?? show).voteAverage)
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
                Task { await preparePlayback(startSeconds: playbackSelection?.startSeconds ?? 0) }
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
                    Task { await preparePlayback(startSeconds: 0) }
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
        if isPreparing { return "Preparing Stream…" }
        guard let playbackSelection else { return "Finding Your Episode…" }
        return "\(playbackSelection.isResume ? "Resume" : "Play") \(playbackSelection.episode.label)"
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
        defer { isPreparing = false }
        let movie = playbackSelection.episode.playbackMovie(in: details.show)
        do {
            let prepared = try await model.preparePlayback(
                for: movie,
                startSeconds: startSeconds
            )
            model.presentPlayback(movie: movie, prepared: prepared)
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
            await refreshPlaybackSelection()
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
