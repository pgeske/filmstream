import FilmstreamCore
import SwiftUI

struct IOSShowEpisodesView: View {
    @Environment(IOSAppModel.self) private var model
    let details: SeriesDetails

    @State private var selectedSeasonNumber: Int
    @State private var loadedSeason: ShowSeason?
    @State private var activePlayback: IOSEpisodeBrowserPlaybackSession?
    @State private var isLoading = false
    @State private var preparingEpisodeID: String?
    @State private var errorMessage: String?

    init(details: SeriesDetails) {
        self.details = details
        _selectedSeasonNumber = State(initialValue: details.seasons.first?.number ?? 1)
    }

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 16) {
                    seasonPicker

                    if let errorMessage {
                        Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                            .font(.footnote.weight(.semibold))
                            .foregroundStyle(Color.mobileTeaAmber)
                    }

                    if isLoading {
                        HStack(spacing: 10) {
                            ProgressView()
                                .tint(Color.mobileTeaAccent)
                            Text("Loading episodes…")
                                .foregroundStyle(Color.mobileTeaMuted)
                        }
                        .frame(maxWidth: .infinity, minHeight: 160)
                    } else {
                        ForEach(loadedSeason?.episodes ?? []) { episode in
                            episodeCard(episode)
                        }
                    }
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 20)
            }
        }
        .navigationTitle("Episodes & More")
        .navigationBarTitleDisplayMode(.inline)
        .toolbarBackground(Color.mobileTeaBackground.opacity(0.94), for: .navigationBar)
        .task(id: selectedSeasonNumber) {
            await loadSelectedSeason()
        }
        .fullScreenCover(
            item: $activePlayback,
            onDismiss: {
                Task { await model.loadContinueWatching() }
            }
        ) { session in
            IOSPlayerView(movie: session.movie, prepared: session.prepared, api: model.api)
        }
    }

    private var seasonPicker: some View {
        VStack(alignment: .leading, spacing: 9) {
            Text(details.show.title)
                .font(.system(size: 27, weight: .black, design: .rounded))
                .foregroundStyle(Color.mobileTeaCream)
                .lineLimit(2)

            Picker("Season", selection: $selectedSeasonNumber) {
                ForEach(details.seasons) { season in
                    Text("\(season.name) · \(season.episodeCount) episodes")
                        .tag(season.number)
                }
            }
            .pickerStyle(.menu)
            .tint(Color.mobileTeaAccentLight)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func episodeCard(_ episode: Episode) -> some View {
        let history = history(for: episode)
        return Button {
            Task { await preparePlayback(for: episode) }
        } label: {
            VStack(alignment: .leading, spacing: 12) {
                MobileEpisodeStillImage(episode: episode)
                    .aspectRatio(16 / 9, contentMode: .fit)
                    .clipShape(RoundedRectangle(cornerRadius: 16, style: .continuous))
                    .overlay {
                        if preparingEpisodeID == episode.id {
                            ZStack {
                                Color.black.opacity(0.5)
                                ProgressView()
                                    .tint(.white)
                            }
                        }
                    }
                    .overlay(alignment: .bottom) {
                        if let history, history.progress > 0 {
                            ProgressView(value: history.progress)
                                .tint(Color.mobileTeaAccent)
                                .padding(.horizontal, 12)
                                .padding(.bottom, 9)
                        }
                    }

                HStack(alignment: .firstTextBaseline, spacing: 9) {
                    Text(String(episode.episodeNumber))
                        .font(.headline.monospacedDigit().weight(.bold))
                        .foregroundStyle(Color.mobileTeaAccentLight)
                    Text(episode.title)
                        .font(.headline.weight(.bold))
                        .foregroundStyle(Color.mobileTeaCream)
                        .multilineTextAlignment(.leading)
                    Spacer()
                    if let runtime = episode.runtimeMinutes, runtime > 0 {
                        Text("\(runtime)m")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(Color.mobileTeaMuted)
                    }
                }

                if let overview = episode.overview, !overview.isEmpty {
                    Text(overview)
                        .font(.subheadline)
                        .foregroundStyle(Color.mobileTeaCream.opacity(0.72))
                        .lineSpacing(3)
                        .lineLimit(4)
                        .multilineTextAlignment(.leading)
                }

                if let history, history.progress > 0 {
                    Text(history.completed ? "Watched" : "\(Int(history.progress * 100))% watched")
                        .font(.caption.weight(.bold))
                        .foregroundStyle(Color.mobileTeaAccentLight)
                }
            }
            .padding(13)
            .background(
                Color.mobileTeaPanel.opacity(0.78),
                in: RoundedRectangle(cornerRadius: 20, style: .continuous)
            )
            .overlay {
                RoundedRectangle(cornerRadius: 20, style: .continuous)
                    .stroke(Color.mobileTeaCream.opacity(0.09), lineWidth: 1)
            }
        }
        .buttonStyle(.plain)
        .disabled(preparingEpisodeID != nil)
    }

    private func history(for episode: Episode) -> WatchHistoryEntry? {
        model.watchHistory.first { $0.mediaID == episode.id }
    }

    private func loadSelectedSeason() async {
        isLoading = true
        defer { isLoading = false }
        do {
            loadedSeason = try await model.api.season(selectedSeasonNumber, for: details.show.id)
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    private func preparePlayback(for episode: Episode) async {
        preparingEpisodeID = episode.id
        defer { preparingEpisodeID = nil }
        let movie = episode.playbackMovie(in: details.show)
        let startSeconds = history(for: episode).flatMap {
            !$0.completed && $0.positionSeconds >= 30 ? $0.positionSeconds : nil
        } ?? 0
        do {
            let prepared = try await model.preparePlayback(
                for: movie,
                startSeconds: startSeconds
            )
            activePlayback = IOSEpisodeBrowserPlaybackSession(movie: movie, prepared: prepared)
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

private struct MobileEpisodeStillImage: View {
    let episode: Episode

    var body: some View {
        AsyncImage(url: episode.stillURL) { phase in
            switch phase {
            case let .success(image):
                image
                    .resizable()
                    .scaledToFill()
            default:
                ZStack {
                    LinearGradient(
                        colors: [Color.mobileTeaPanelElevated, Color.mobileTeaPanel],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    )
                    Image(systemName: "play.rectangle.fill")
                        .font(.system(size: 38))
                        .foregroundStyle(Color.mobileTeaAccent.opacity(0.72))
                }
            }
        }
    }
}

private struct IOSEpisodeBrowserPlaybackSession: Identifiable {
    let movie: Movie
    let prepared: PreparedPlayback

    var id: String { prepared.id }
}
