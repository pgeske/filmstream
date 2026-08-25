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
    @State private var preparationStage: PlaybackPreparationStage?
    @State private var errorMessage: String?

    init(details: SeriesDetails) {
        self.details = details
        _selectedSeasonNumber = State(initialValue: details.seasons.first?.number ?? 1)
    }

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 18) {
                    seasonPicker

                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        Text(loadedSeason?.name ?? "Season \(selectedSeasonNumber)")
                            .font(.title2.weight(.bold))
                            .foregroundStyle(Color.mobileTeaCream)

                        Spacer()

                        if let preparationStage {
                            HStack(spacing: 8) {
                                ProgressView()
                                    .controlSize(.small)
                                    .tint(Color.mobileTeaAccent)
                                Text(preparationStageLabel(preparationStage))
                                    .font(.caption.weight(.bold))
                                    .foregroundStyle(Color.mobileTeaAccentLight)
                            }
                        }
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
        .toolbar(.visible, for: .navigationBar)
        .toolbarBackground(Color.mobileTeaBackground.opacity(0.94), for: .navigationBar)
        .toolbarBackground(.visible, for: .navigationBar)
        .toolbarColorScheme(.dark, for: .navigationBar)
        .task(id: selectedSeasonNumber) {
            await loadSelectedSeason()
        }
        .fullScreenCover(
            item: $activePlayback,
            onDismiss: {
                Task { await model.loadContinueWatching() }
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

    private var seasonPicker: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 12) {
                MobileTeaStreamMark(size: 38)
                VStack(alignment: .leading, spacing: 2) {
                    Text(details.show.title)
                        .font(.system(size: 27, weight: .black, design: .rounded))
                        .foregroundStyle(Color.mobileTeaCream)
                        .lineLimit(2)
                    Text(details.show.seasonCountLabel ?? "Episodes")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(Color.mobileTeaMuted)
                }
            }

            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 10) {
                    ForEach(details.seasons) { season in
                        seasonButton(season)
                    }
                }
                .padding(.vertical, 2)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func seasonButton(_ season: SeasonSummary) -> some View {
        let isSelected = selectedSeasonNumber == season.number
        return Button {
            withAnimation(.snappy(duration: 0.2)) {
                selectedSeasonNumber = season.number
            }
        } label: {
            VStack(alignment: .leading, spacing: 2) {
                Text(season.name)
                    .font(.subheadline.weight(.bold))
                    .foregroundStyle(isSelected ? Color.mobileTeaBackground : Color.mobileTeaCream)
                Text("\(season.episodeCount) episodes")
                    .font(.caption2.weight(.semibold))
                    .foregroundStyle(
                        isSelected
                            ? Color.mobileTeaBackground.opacity(0.7)
                            : Color.mobileTeaMuted
                    )
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
            .background(
                isSelected ? Color.mobileTeaAccentLight : Color.mobileTeaPanel.opacity(0.78),
                in: RoundedRectangle(cornerRadius: 13, style: .continuous)
            )
            .overlay {
                RoundedRectangle(cornerRadius: 13, style: .continuous)
                    .stroke(
                        isSelected ? Color.mobileTeaCream.opacity(0.28) : Color.mobileTeaCream.opacity(0.09),
                        lineWidth: 1
                    )
            }
        }
        .buttonStyle(MobileCardButtonStyle())
        .accessibilityAddTraits(isSelected ? .isSelected : [])
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
                                Color.black.opacity(0.52)
                                ProgressView()
                                    .controlSize(.large)
                                    .tint(Color.mobileTeaAccentLight)
                            }
                        } else {
                            Image(systemName: "play.fill")
                                .font(.system(size: 18, weight: .bold))
                                .foregroundStyle(Color.mobileTeaCream)
                                .frame(width: 50, height: 50)
                                .background(Color.mobileTeaBackground.opacity(0.84), in: Circle())
                                .overlay {
                                    Circle()
                                        .stroke(Color.mobileTeaAccentLight.opacity(0.72), lineWidth: 1.5)
                                }
                                .shadow(color: .black.opacity(0.4), radius: 10, y: 5)
                                .accessibilityHidden(true)
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
                LinearGradient(
                    colors: [Color.mobileTeaPanelElevated.opacity(0.68), Color.mobileTeaPanel.opacity(0.72)],
                    startPoint: .topLeading,
                    endPoint: .bottomTrailing
                ),
                in: RoundedRectangle(cornerRadius: 20, style: .continuous)
            )
            .overlay {
                RoundedRectangle(cornerRadius: 20, style: .continuous)
                    .stroke(Color.mobileTeaCream.opacity(0.09), lineWidth: 1)
            }
            .shadow(color: .black.opacity(0.16), radius: 10, y: 5)
        }
        .buttonStyle(MobileCardButtonStyle())
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

    private func preparationStageLabel(_ stage: PlaybackPreparationStage) -> String {
        switch stage {
        case .findingRelease:
            "Finding Release…"
        case .bufferingVideo:
            "Buffering Episode…"
        }
    }

    private func preparePlayback(for episode: Episode) async {
        preparingEpisodeID = episode.id
        defer {
            preparingEpisodeID = nil
            preparationStage = nil
        }
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
                onStage: { preparationStage = $0 }
            )
            let nextEpisode = await nextEpisodeTask.value
            activePlayback = IOSEpisodeBrowserPlaybackSession(
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
        activePlayback = IOSEpisodeBrowserPlaybackSession(
            movie: movie,
            prepared: prepared,
            nextEpisode: nextEpisode
        )
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
    let nextEpisode: Episode?

    var id: String { prepared.id }
}
