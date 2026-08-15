import FilmstreamCore
import SwiftUI

struct MacMovieDetailView: View {
    @Environment(MacAppModel.self) private var model
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
        ZStack {
            MacTeaBackground()
            ScrollView {
                VStack(spacing: 0) {
                    backdrop
                    detailContent
                        .padding(.horizontal, 42)
                        .offset(y: -92)
                        .padding(.bottom, -48)
                }
            }
        }
        .navigationTitle(movie.title)
        .task(id: movie.id) {
            await model.loadRatings(for: movie)
        }
        .sheet(item: $preparedPlayback, onDismiss: {
            Task { await model.loadContinueWatching() }
        }) { prepared in
            MacPlayerView(movie: movie, prepared: prepared, api: model.api)
                .frame(minWidth: 900, minHeight: 580)
        }
    }

    private var backdrop: some View {
        MacBackdropImage(movie: movie)
            .frame(maxWidth: .infinity)
            .frame(height: 390)
            .clipped()
            .overlay {
                LinearGradient(
                    stops: [
                        .init(color: .clear, location: 0.2),
                        .init(color: Color.macTeaBackground.opacity(0.72), location: 0.72),
                        .init(color: Color.macTeaBackground, location: 1),
                    ],
                    startPoint: .top,
                    endPoint: .bottom
                )
            }
            .overlay {
                LinearGradient(
                    colors: [Color.macTeaBackground.opacity(0.72), .clear],
                    startPoint: .leading,
                    endPoint: .trailing
                )
            }
    }

    private var detailContent: some View {
        HStack(alignment: .bottom, spacing: 34) {
            MacPosterImage(movie: movie)
                .frame(width: 230, height: 345)
                .overlay {
                    RoundedRectangle(cornerRadius: 14, style: .continuous)
                        .stroke(Color.macTeaAccentLight.opacity(0.24), lineWidth: 1)
                }
                .shadow(color: .black.opacity(0.48), radius: 24, y: 12)

            VStack(alignment: .leading, spacing: 16) {
                Text(movie.title)
                    .font(.system(size: 40, weight: .bold, design: .rounded))
                    .foregroundStyle(Color.macTeaCream)
                    .lineLimit(2)

                metadataLine

                if let overview = movie.overview, !overview.isEmpty {
                    Text(overview)
                        .font(.body)
                        .foregroundStyle(Color.macTeaCream.opacity(0.84))
                        .lineSpacing(3)
                        .frame(maxWidth: 720, alignment: .leading)
                }

                if let errorMessage {
                    Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(Color.macTeaAmber)
                        .font(.headline)
                }

                Button {
                    Task { await preparePlayback() }
                } label: {
                    HStack(spacing: 9) {
                        if isPreparing {
                            ProgressView()
                                .controlSize(.small)
                                .tint(Color.macTeaBackground)
                        } else {
                            Image(systemName: "play.fill")
                        }
                        Text(playButtonTitle)
                    }
                    .font(.headline.weight(.bold))
                }
                .buttonStyle(MacTeaActionButtonStyle(prominent: true))
                .disabled(isPreparing)
                .keyboardShortcut(.defaultAction)
            }
            .padding(.bottom, 12)

            Spacer(minLength: 0)
        }
        .frame(maxWidth: 1_080, alignment: .leading)
    }

    @ViewBuilder
    private var metadataLine: some View {
        HStack(spacing: 14) {
            if let year = movie.year {
                Text(String(year))
            }
            MacMovieRatingBadges(ratings: ratings, tmdbRating: movie.voteAverage)
            if let history, history.progress > 0 {
                Text("\(Int(history.progress * 100))% watched")
                    .foregroundStyle(Color.macTeaAccentLight)
            }
        }
        .font(.headline)
        .foregroundStyle(Color.macTeaMuted)
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
