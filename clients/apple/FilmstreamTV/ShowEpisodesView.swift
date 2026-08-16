import FilmstreamCore
import SwiftUI

struct ShowEpisodesView: View {
    @Environment(AppModel.self) private var model
    let details: SeriesDetails

    @State private var selectedSeasonNumber: Int
    @State private var loadedSeason: ShowSeason?
    @State private var activePlayback: TVEpisodeBrowserPlaybackSession?
    @State private var isLoading = false
    @State private var preparingEpisodeID: String?
    @State private var errorMessage: String?
    @FocusState private var focusedSeasonNumber: Int?
    @FocusState private var focusedEpisodeID: String?

    init(details: SeriesDetails) {
        self.details = details
        _selectedSeasonNumber = State(initialValue: details.seasons.first?.number ?? 1)
    }

    var body: some View {
        ZStack {
            TeaBackground()

            HStack(alignment: .top, spacing: 42) {
                seasonSidebar
                    .frame(width: 430)

                episodeList
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            .padding(.horizontal, 72)
            .padding(.top, 48)
            .padding(.bottom, 54)
        }
        .navigationTitle("Episodes & More")
        .task(id: selectedSeasonNumber) {
            await loadSelectedSeason()
        }
        .onAppear {
            focusedSeasonNumber = selectedSeasonNumber
        }
        .fullScreenCover(
            item: $activePlayback,
            onDismiss: {
                Task { await model.loadContinueWatching() }
            }
        ) { session in
            PlayerView(movie: session.movie, prepared: session.prepared, api: model.api)
        }
    }

    private var seasonSidebar: some View {
        VStack(alignment: .leading, spacing: 20) {
            TeaStreamMark(size: 42)

            Text(details.show.title)
                .font(.system(size: 36, weight: .black, design: .rounded))
                .foregroundStyle(Color.teaCream)
                .lineLimit(3)

            Text(details.show.seasonCountLabel ?? "Episodes")
                .font(.headline)
                .foregroundStyle(Color.teaMuted)

            ScrollView(.vertical, showsIndicators: false) {
                VStack(alignment: .leading, spacing: 8) {
                    ForEach(details.seasons) { season in
                        seasonButton(season)
                    }
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 8)
            }
        }
    }

    private func seasonButton(_ season: SeasonSummary) -> some View {
        let isSelected = selectedSeasonNumber == season.number
        let isFocused = focusedSeasonNumber == season.number
        return Button {
            selectedSeasonNumber = season.number
        } label: {
            HStack(spacing: 14) {
                RoundedRectangle(cornerRadius: 2, style: .continuous)
                    .fill(isSelected ? Color.teaAccentLight : Color.teaCream.opacity(0.12))
                    .frame(width: 4, height: 38)

                Text(season.name)
                    .font(.headline.weight(.bold))
                    .lineLimit(1)
                    .layoutPriority(1)

                Spacer(minLength: 8)

                Text("\(season.episodeCount) episodes")
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(isFocused ? Color.teaCream.opacity(0.82) : Color.teaMuted)
                    .lineLimit(1)
                    .fixedSize(horizontal: true, vertical: false)
            }
            .foregroundStyle(isFocused || isSelected ? Color.teaCream : Color.teaCream.opacity(0.7))
            .padding(.horizontal, 16)
            .padding(.vertical, 14)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background {
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .fill(
                        isFocused
                            ? Color.teaPanelElevated
                            : Color.teaPanel.opacity(isSelected ? 0.82 : 0.22)
                    )
            }
            .overlay {
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .stroke(
                        isFocused ? Color.teaAccentLight.opacity(0.78) : Color.clear,
                        lineWidth: 2
                    )
            }
            .shadow(
                color: isFocused ? Color.teaAccent.opacity(0.2) : .clear,
                radius: 18,
                y: 8
            )
            .scaleEffect(isFocused ? 1.025 : 1, anchor: .leading)
            .animation(.snappy(duration: 0.2), value: isFocused)
        }
        .buttonStyle(EpisodeBrowserButtonStyle())
        .focusEffectDisabled()
        .focused($focusedSeasonNumber, equals: season.number)
    }

    @ViewBuilder
    private var episodeList: some View {
        VStack(alignment: .leading, spacing: 18) {
            HStack(alignment: .firstTextBaseline) {
                Text(loadedSeason?.name ?? "Season \(selectedSeasonNumber)")
                    .font(.system(size: 42, weight: .bold, design: .rounded))
                    .foregroundStyle(Color.teaCream)
                Spacer()
                if isLoading {
                    ProgressView()
                        .tint(Color.teaAccent)
                }
            }

            if let errorMessage {
                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                    .font(.headline)
                    .foregroundStyle(Color.teaAmber)
            }

            ScrollView(.vertical, showsIndicators: false) {
                LazyVStack(spacing: 14) {
                    ForEach(loadedSeason?.episodes ?? []) { episode in
                        episodeButton(episode)
                    }
                }
                .padding(.horizontal, 8)
                .padding(.vertical, 8)
            }
        }
    }

    private func episodeButton(_ episode: Episode) -> some View {
        let history = history(for: episode)
        let isFocused = focusedEpisodeID == episode.id
        return Button {
            Task { await preparePlayback(for: episode) }
        } label: {
            HStack(spacing: 24) {
                EpisodeStillImage(episode: episode)
                    .frame(width: 310, height: 174)
                    .clipShape(RoundedRectangle(cornerRadius: 16, style: .continuous))
                    .overlay(alignment: .center) {
                        if preparingEpisodeID == episode.id {
                            ZStack {
                                Color.black.opacity(0.48)
                                ProgressView()
                                    .tint(.white)
                            }
                        } else if isFocused {
                            Image(systemName: "play.fill")
                                .font(.system(size: 27, weight: .bold))
                                .foregroundStyle(Color.teaCream)
                                .padding(18)
                                .background(Color.teaBackground.opacity(0.88), in: Circle())
                                .overlay {
                                    Circle()
                                        .stroke(Color.teaAccentLight.opacity(0.9), lineWidth: 2)
                                }
                                .shadow(color: .black.opacity(0.34), radius: 12, y: 6)
                        }
                    }

                VStack(alignment: .leading, spacing: 9) {
                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        Text("\(episode.episodeNumber)")
                            .font(.title2.monospacedDigit().weight(.bold))
                            .foregroundStyle(Color.teaAccentLight)
                        Text(episode.title)
                            .font(.title3.weight(.bold))
                            .foregroundStyle(Color.teaCream)
                            .lineLimit(1)
                        Spacer()
                        if let runtime = episode.runtimeMinutes, runtime > 0 {
                            Text("\(runtime)m")
                                .font(.subheadline.weight(.semibold))
                                .foregroundStyle(Color.teaMuted)
                        }
                    }

                    if let overview = episode.overview, !overview.isEmpty {
                        Text(overview)
                            .font(.body)
                            .foregroundStyle(Color.teaCream.opacity(0.72))
                            .lineSpacing(3)
                            .lineLimit(3)
                    }

                    if let history, history.progress > 0 {
                        HStack(spacing: 12) {
                            ProgressView(value: history.progress)
                                .tint(Color.teaAccent)
                            Text(history.completed ? "Watched" : "\(Int(history.progress * 100))%")
                                .font(.caption.weight(.bold))
                                .foregroundStyle(Color.teaAccentLight)
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .padding(14)
            .background {
                RoundedRectangle(cornerRadius: 21, style: .continuous)
                    .fill(isFocused ? Color.teaPanelElevated : Color.teaPanel.opacity(0.38))
            }
            .overlay {
                RoundedRectangle(cornerRadius: 21, style: .continuous)
                    .stroke(
                        isFocused ? Color.teaAccentLight.opacity(0.78) : Color.teaCream.opacity(0.07),
                        lineWidth: isFocused ? 2 : 1
                    )
            }
            .shadow(
                color: isFocused ? Color.teaAccent.opacity(0.18) : .black.opacity(0.12),
                radius: isFocused ? 20 : 8,
                y: isFocused ? 9 : 4
            )
            .scaleEffect(isFocused ? 1.012 : 1)
            .animation(.snappy(duration: 0.2), value: isFocused)
        }
        .buttonStyle(EpisodeBrowserButtonStyle())
        .focusEffectDisabled()
        .focused($focusedEpisodeID, equals: episode.id)
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
                startSeconds: startSeconds,
                onStage: { _ in }
            )
            activePlayback = TVEpisodeBrowserPlaybackSession(movie: movie, prepared: prepared)
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

private struct EpisodeBrowserButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .opacity(configuration.isPressed ? 0.86 : 1)
            .scaleEffect(configuration.isPressed ? 0.99 : 1)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
    }
}

private struct EpisodeStillImage: View {
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
                        colors: [Color.teaPanelElevated, Color.teaPanel],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    )
                    Image(systemName: "play.rectangle.fill")
                        .font(.system(size: 42))
                        .foregroundStyle(Color.teaAccent.opacity(0.7))
                }
            }
        }
    }
}

private struct TVEpisodeBrowserPlaybackSession: Identifiable {
    let movie: Movie
    let prepared: PreparedPlayback

    var id: String { prepared.id }
}
