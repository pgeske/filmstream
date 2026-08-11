import FilmstreamCore
import Foundation
import SwiftUI
import UIKit

extension Color {
    static let filmstreamBackground = Color(red: 0.035, green: 0.045, blue: 0.07)
    static let filmstreamBackgroundTop = Color(red: 0.055, green: 0.07, blue: 0.11)
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

private struct MovieCardButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .opacity(configuration.isPressed ? 0.82 : 1)
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

struct MovieNavigationCard: View {
    let movie: Movie
    var progress: Double? = nil
    var requestsInitialFocus = false
    var action: (() -> Void)? = nil
    @FocusState private var isFocused: Bool

    private let cardWidth: CGFloat = 250

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
        .buttonStyle(MovieCardButtonStyle())
        .focusEffectDisabled()
        .focused($isFocused)
        .zIndex(isFocused ? 1 : 0)
        .task {
            if requestsInitialFocus {
                isFocused = true
            }
        }
    }

    private var cardContent: some View {
        VStack(alignment: .leading, spacing: 13) {
            PosterImage(movie: movie)
                .frame(width: cardWidth, height: 375)
                .overlay {
                    RoundedRectangle(cornerRadius: 20, style: .continuous)
                        .stroke(
                            isFocused ? Color.filmstreamAccent : .white.opacity(0.09),
                            lineWidth: isFocused ? 3 : 1
                        )
                }
                .overlay(alignment: .bottom) {
                    if let progress, progress > 0 {
                        ProgressView(value: progress)
                            .tint(Color.filmstreamAccent)
                            .padding(.horizontal, 12)
                            .padding(.bottom, 11)
                    }
                }
                .shadow(
                    color: isFocused ? Color.filmstreamAccent.opacity(0.2) : .black.opacity(0.35),
                    radius: isFocused ? 28 : 12,
                    y: isFocused ? 14 : 8
                )

            Text(movie.title)
                .font(.headline.weight(.semibold))
                .foregroundStyle(.white)
                .lineLimit(2)
                .frame(width: cardWidth, height: 58, alignment: .topLeading)
            if let year = movie.year {
                Text(String(year))
                    .font(.subheadline)
                    .foregroundStyle(.white.opacity(0.58))
            }
        }
        .frame(width: cardWidth, alignment: .leading)
        .contentShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .scaleEffect(isFocused ? 1.035 : 1)
        .offset(y: isFocused ? -6 : 0)
        .animation(.snappy(duration: 0.22), value: isFocused)
    }
}

struct PosterImage: View {
    let movie: Movie

    @State private var imageData: Data?
    @State private var isLoading = false

    var body: some View {
        Group {
            if let imageData, let image = UIImage(data: imageData) {
                Image(uiImage: image)
                    .resizable()
                    .scaledToFill()
            } else if isLoading {
                ZStack {
                    Color.filmstreamPanel
                    ProgressView()
                }
            } else {
                placeholder
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .task(id: movie) {
            await loadArtwork()
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
                .foregroundStyle(.white.opacity(0.32))
        }
    }

    private func loadArtwork() async {
        imageData = nil
        let urls = [movie.posterURL, movie.backdropURL].compactMap { $0 }
        guard !urls.isEmpty else { return }

        isLoading = true
        defer { isLoading = false }
        for url in urls {
            for attempt in 0..<2 {
                do {
                    var request = URLRequest(
                        url: url,
                        cachePolicy: attempt == 0 ? .returnCacheDataElseLoad : .reloadIgnoringLocalCacheData,
                        timeoutInterval: 20
                    )
                    request.setValue("image/avif,image/webp,image/*,*/*;q=0.8", forHTTPHeaderField: "Accept")
                    let (data, response) = try await URLSession.shared.data(for: request)
                    guard let response = response as? HTTPURLResponse,
                          (200..<300).contains(response.statusCode),
                          UIImage(data: data) != nil else {
                        continue
                    }
                    imageData = data
                    return
                } catch is CancellationError {
                    return
                } catch {
                    if attempt == 0 {
                        try? await Task.sleep(for: .milliseconds(350))
                    }
                }
            }
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
