import FilmstreamCore
import SwiftUI

struct MacShowEpisodesView: View {
    @Environment(MacAppModel.self) private var model
    let details: SeriesDetails

    @State private var selectedSeasonNumber: Int
    @State private var loadedSeason: ShowSeason?
    @State private var isLoading = false
    @State private var preparingEpisodeID: String?
    @State private var hoveredEpisodeID: String?
    @State private var errorMessage: String?

    init(details: SeriesDetails) {
        self.details = details
        _selectedSeasonNumber = State(initialValue: details.seasons.first?.number ?? 1)
    }

    var body: some View {
        ZStack {
            MacTeaBackground()

            HStack(alignment: .top, spacing: 28) {
                seasonSidebar
                    .frame(width: 220)

                episodeList
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            .padding(28)
        }
        .navigationTitle("Episodes & More")
        .task(id: selectedSeasonNumber) {
            await loadSelectedSeason()
        }
    }

    private var seasonSidebar: some View {
        VStack(alignment: .leading, spacing: 16) {
            MacTeaStreamMark(size: 36)

            Text(details.show.title)
                .font(.system(size: 25, weight: .black, design: .rounded))
                .foregroundStyle(Color.macTeaCream)
                .lineLimit(3)

            Text(details.show.seasonCountLabel ?? "Episodes")
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(Color.macTeaMuted)

            ScrollView {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(details.seasons) { season in
                        Button {
                            selectedSeasonNumber = season.number
                        } label: {
                            HStack(spacing: 10) {
                                Image(systemName: selectedSeasonNumber == season.number ? "play.fill" : "circle.fill")
                                    .font(.caption2)
                                    .foregroundStyle(Color.macTeaAccentLight)
                                    .frame(width: 14)
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(season.name)
                                        .font(.headline)
                                    Text("\(season.episodeCount) episodes")
                                        .font(.caption)
                                        .foregroundStyle(Color.macTeaMuted)
                                }
                                Spacer()
                            }
                            .foregroundStyle(Color.macTeaCream)
                            .padding(.horizontal, 12)
                            .padding(.vertical, 10)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .background(
                                selectedSeasonNumber == season.number
                                    ? Color.macTeaPanelElevated
                                    : Color.clear,
                                in: RoundedRectangle(cornerRadius: 10, style: .continuous)
                            )
                        }
                        .buttonStyle(.plain)
                    }
                }
            }
        }
    }

    private var episodeList: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack(alignment: .firstTextBaseline) {
                Text(loadedSeason?.name ?? "Season \(selectedSeasonNumber)")
                    .font(.system(size: 34, weight: .bold, design: .rounded))
                    .foregroundStyle(Color.macTeaCream)
                Spacer()
                if isLoading {
                    ProgressView()
                        .controlSize(.small)
                        .tint(Color.macTeaAccent)
                }
            }

            if let errorMessage {
                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                    .font(.headline)
                    .foregroundStyle(Color.macTeaAmber)
            }

            ScrollView {
                LazyVStack(spacing: 12) {
                    ForEach(loadedSeason?.episodes ?? []) { episode in
                        episodeCard(episode)
                    }
                }
                .padding(.horizontal, 3)
                .padding(.vertical, 4)
            }
        }
    }

    private func episodeCard(_ episode: Episode) -> some View {
        let history = history(for: episode)
        let isHovered = hoveredEpisodeID == episode.id
        return Button {
            Task { await preparePlayback(for: episode) }
        } label: {
            HStack(spacing: 18) {
                MacEpisodeStillImage(episode: episode)
                    .frame(width: 260, height: 146)
                    .clipShape(RoundedRectangle(cornerRadius: 13, style: .continuous))
                    .overlay {
                        if preparingEpisodeID == episode.id {
                            ZStack {
                                Color.black.opacity(0.5)
                                ProgressView()
                                    .tint(.white)
                            }
                        } else if isHovered {
                            Image(systemName: "play.fill")
                                .font(.headline.weight(.bold))
                                .foregroundStyle(Color.macTeaBackground)
                                .padding(13)
                                .background(Color.macTeaCream, in: Circle())
                        }
                    }

                VStack(alignment: .leading, spacing: 7) {
                    HStack(alignment: .firstTextBaseline, spacing: 10) {
                        Text(String(episode.episodeNumber))
                            .font(.headline.monospacedDigit().weight(.bold))
                            .foregroundStyle(Color.macTeaAccentLight)
                        Text(episode.title)
                            .font(.headline.weight(.bold))
                            .foregroundStyle(Color.macTeaCream)
                            .lineLimit(1)
                        Spacer()
                        if let runtime = episode.runtimeMinutes, runtime > 0 {
                            Text("\(runtime)m")
                                .font(.caption.weight(.semibold))
                                .foregroundStyle(Color.macTeaMuted)
                        }
                    }

                    if let overview = episode.overview, !overview.isEmpty {
                        Text(overview)
                            .font(.subheadline)
                            .foregroundStyle(Color.macTeaCream.opacity(0.7))
                            .lineSpacing(2)
                            .lineLimit(3)
                            .multilineTextAlignment(.leading)
                    }

                    if let history, history.progress > 0 {
                        HStack(spacing: 10) {
                            ProgressView(value: history.progress)
                                .tint(Color.macTeaAccent)
                            Text(history.completed ? "Watched" : "\(Int(history.progress * 100))%")
                                .font(.caption.weight(.bold))
                                .foregroundStyle(Color.macTeaAccentLight)
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .padding(12)
            .background(
                isHovered ? Color.macTeaPanelElevated : Color.macTeaPanel.opacity(0.56),
                in: RoundedRectangle(cornerRadius: 17, style: .continuous)
            )
            .overlay {
                RoundedRectangle(cornerRadius: 17, style: .continuous)
                    .stroke(
                        isHovered ? Color.macTeaAccentLight.opacity(0.55) : Color.macTeaCream.opacity(0.07),
                        lineWidth: 1
                    )
            }
        }
        .buttonStyle(.plain)
        .onHover { isHovering in
            hoveredEpisodeID = isHovering ? episode.id : nil
        }
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
            let nextEpisodeTask = Task {
                try? await model.api.nextEpisode(after: episode, in: details)
            }
            let prepared = try await model.preparePlayback(
                for: movie,
                startSeconds: startSeconds,
                onStage: { _ in }
            )
            model.presentPlayback(
                movie: movie,
                prepared: prepared,
                details: details,
                nextEpisode: await nextEpisodeTask.value
            )
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

private struct MacEpisodeStillImage: View {
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
                        colors: [Color.macTeaPanelElevated, Color.macTeaPanel],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    )
                    Image(systemName: "play.rectangle.fill")
                        .font(.system(size: 34))
                        .foregroundStyle(Color.macTeaAccent.opacity(0.72))
                }
            }
        }
    }
}
