import FilmstreamCore
import SwiftUI

struct MovieDetailView: View {
    @Environment(AppModel.self) private var model
    let movie: Movie

    @State private var preparedPlayback: PreparedPlayback?
    @State private var isPreparing = false
    @State private var errorMessage: String?

    private var history: WatchHistoryEntry? {
        model.history(for: movie)
    }

    private var ratings: MovieRatings? {
        model.ratings(for: movie)
    }

    var body: some View {
        ZStack(alignment: .bottomLeading) {
            BackdropImage(movie: movie)
                .ignoresSafeArea()
                .overlay {
                    LinearGradient(
                        stops: [
                            .init(color: .black.opacity(0.05), location: 0),
                            .init(color: Color.teaBackground.opacity(0.82), location: 0.62),
                            .init(color: Color.teaBackground, location: 1),
                        ],
                        startPoint: .top,
                        endPoint: .bottom
                    )
                }
                .overlay {
                    LinearGradient(
                        colors: [Color.teaBackground.opacity(0.94), .clear],
                        startPoint: .leading,
                        endPoint: .trailing
                    )
                }

            HStack(alignment: .center, spacing: 54) {
                PosterImage(movie: movie)
                    .frame(width: 310, height: 465)
                    .overlay {
                        RoundedRectangle(cornerRadius: 20, style: .continuous)
                            .stroke(Color.teaAccentLight.opacity(0.24), lineWidth: 1.5)
                    }
                    .shadow(color: Color.teaAccent.opacity(0.14), radius: 32, y: 14)

                VStack(alignment: .leading, spacing: 22) {
                    Text(movie.title)
                        .font(.system(size: 58, weight: .bold, design: .rounded))
                        .foregroundStyle(Color.teaCream)
                        .lineLimit(2)

                    metadataLine

                    if let overview = movie.overview, !overview.isEmpty {
                        Text(overview)
                            .font(.title3)
                            .foregroundStyle(Color.teaCream.opacity(0.86))
                            .lineLimit(5)
                            .frame(maxWidth: 900, alignment: .leading)
                    }

                    if let errorMessage {
                        Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(Color.teaAmber)
                            .font(.headline)
                    }

                    Button {
                        Task { await preparePlayback() }
                    } label: {
                        HStack(spacing: 12) {
                            if isPreparing {
                                ProgressView()
                                    .tint(Color.teaBackground)
                            } else {
                                Image(systemName: "play.fill")
                            }
                            Text(playButtonTitle)
                        }
                        .font(.title3.weight(.bold))
                    }
                    .buttonStyle(TeaActionButtonStyle(prominent: true))
                    .focusEffectDisabled()
                    .disabled(isPreparing)
                }
                .padding(.bottom, 22)

                Spacer()
            }
            .padding(.horizontal, 76)
            .padding(.bottom, 62)
        }
        .background(Color.teaBackground)
        .task(id: movie.id) {
            await model.loadRatings(for: movie)
        }
        .fullScreenCover(
            item: $preparedPlayback,
            onDismiss: {
                Task { await model.loadContinueWatching() }
            }
        ) { prepared in
            PlayerView(movie: movie, prepared: prepared, api: model.api)
        }
    }

    @ViewBuilder
    private var metadataLine: some View {
        HStack(spacing: 16) {
            if let year = movie.year {
                Text(String(year))
            }
            MovieRatingBadges(
                ratings: ratings,
                tmdbRating: movie.voteAverage
            )
            if let history, history.progress > 0 {
                Text("\(Int(history.progress * 100))% watched")
                    .foregroundStyle(Color.teaAccentLight)
            }
        }
        .font(.headline)
        .foregroundStyle(Color.teaMuted)
    }

    private var playButtonTitle: String {
        if isPreparing {
            return "Preparing Stream…"
        }
        return history == nil ? "Play" : "Resume"
    }

    private func preparePlayback() async {
        isPreparing = true
        defer { isPreparing = false }
        do {
            let playback = try await model.api.createPlayback(for: movie)
            preparedPlayback = try await model.api.prepareNativePlayback(
                playback,
                startSeconds: history?.positionSeconds ?? 0
            )
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
