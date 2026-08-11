import FilmstreamCore
import SwiftUI

extension Color {
    static let filmstreamBackground = Color(red: 0.035, green: 0.045, blue: 0.07)
    static let filmstreamPanel = Color(red: 0.075, green: 0.09, blue: 0.13)
    static let filmstreamAccent = Color(red: 0.95, green: 0.23, blue: 0.28)
}

struct BrandHeader: View {
    var body: some View {
        HStack(spacing: 14) {
            Image(systemName: "play.rectangle.fill")
                .foregroundStyle(Color.filmstreamAccent)
                .font(.system(size: 42, weight: .bold))
            Text("FILMSTREAM")
                .font(.system(size: 34, weight: .black, design: .rounded))
                .tracking(2)
        }
        .accessibilityElement(children: .combine)
    }
}

struct MovieNavigationCard: View {
    let movie: Movie
    var progress: Double? = nil
    var action: (() -> Void)? = nil
    @FocusState private var isFocused: Bool

    var body: some View {
        Group {
            if let action {
                Button(action: action) {
                    cardContent
                }
            } else {
                NavigationLink(value: movie) {
                    cardContent
                }
            }
        }
        .buttonStyle(.plain)
        .focused($isFocused)
    }

    private var cardContent: some View {
        VStack(alignment: .leading, spacing: 12) {
            PosterImage(movie: movie)
                .frame(width: 230, height: 345)
                .overlay(alignment: .bottom) {
                    if let progress, progress > 0 {
                        ProgressView(value: progress)
                            .tint(Color.filmstreamAccent)
                            .padding(.horizontal, 10)
                            .padding(.bottom, 9)
                    }
                }
            Text(movie.title)
                .font(.headline)
                .lineLimit(1)
            if let year = movie.year {
                Text(String(year))
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(width: 230, alignment: .leading)
        .scaleEffect(isFocused ? 1.06 : 1)
        .shadow(color: .black.opacity(isFocused ? 0.55 : 0.2), radius: isFocused ? 24 : 8, y: 10)
        .animation(.easeOut(duration: 0.16), value: isFocused)
    }
}

struct PosterImage: View {
    let movie: Movie

    var body: some View {
        Group {
            if let posterURL = movie.posterURL {
                AsyncImage(url: posterURL) { phase in
                    switch phase {
                    case let .success(image):
                        image
                            .resizable()
                            .scaledToFill()
                    case .empty:
                        ZStack {
                            Color.filmstreamPanel
                            ProgressView()
                        }
                    case .failure:
                        placeholder
                    @unknown default:
                        placeholder
                    }
                }
            } else {
                placeholder
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 18, style: .continuous))
        .overlay {
            RoundedRectangle(cornerRadius: 18, style: .continuous)
                .stroke(.white.opacity(0.08), lineWidth: 1)
        }
    }

    private var placeholder: some View {
        ZStack {
            LinearGradient(
                colors: [Color.filmstreamPanel, Color.black.opacity(0.8)],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            Image(systemName: "film.stack")
                .font(.system(size: 54))
                .foregroundStyle(.secondary)
        }
    }
}

struct BackdropImage: View {
    let movie: Movie

    var body: some View {
        AsyncImage(url: movie.backdropURL) { phase in
            if case let .success(image) = phase {
                image
                    .resizable()
                    .scaledToFill()
            } else {
                LinearGradient(
                    colors: [Color.filmstreamPanel, Color.filmstreamBackground],
                    startPoint: .topTrailing,
                    endPoint: .bottomLeading
                )
            }
        }
    }
}
