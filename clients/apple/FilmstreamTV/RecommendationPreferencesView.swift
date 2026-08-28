import SwiftUI

struct RecommendationPreferencesView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var draftPrompt: String
    @State private var isSaving = false
    @State private var errorMessage: String?
    @FocusState private var isEditorFocused: Bool

    private let originalPrompt: String

    init(prompt: String) {
        originalPrompt = prompt
        _draftPrompt = State(initialValue: prompt)
    }

    var body: some View {
        ZStack {
            TeaBackground()

            VStack(alignment: .leading, spacing: 28) {
                VStack(alignment: .leading, spacing: 10) {
                    Text("Your Taste")
                        .font(.system(size: 44, weight: .bold, design: .rounded))
                        .foregroundStyle(Color.teaCream)
                    Text("Describe favorite genres and shows, the pacing you enjoy, and anything you want TeaStream to avoid.")
                        .font(.title3)
                        .foregroundStyle(Color.teaMuted)
                    Text("For example: “Hopeful science fiction, clever mysteries, and slow-burn dramas. Avoid gore and reality TV.”")
                        .font(.headline)
                        .foregroundStyle(Color.teaAccentLight.opacity(0.88))
                }

                TextField(
                    "Describe what you like to watch",
                    text: $draftPrompt,
                    axis: .vertical
                )
                    .font(.title3)
                    .foregroundStyle(Color.teaCream)
                    .textFieldStyle(.plain)
                    .lineLimit(6...10)
                    .padding(16)
                    .frame(minHeight: 230, alignment: .topLeading)
                    .background(
                        Color.teaPanel.opacity(0.9),
                        in: RoundedRectangle(cornerRadius: 20, style: .continuous)
                    )
                    .overlay {
                        RoundedRectangle(cornerRadius: 20, style: .continuous)
                            .stroke(
                                isEditorFocused
                                    ? Color.teaAccentLight
                                    : Color.teaCream.opacity(0.14),
                                lineWidth: isEditorFocused ? 3 : 1
                            )
                    }
                    .focused($isEditorFocused)
                    .accessibilityLabel("Taste description")

                if let errorMessage {
                    Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                        .font(.headline)
                        .foregroundStyle(Color.teaAmber)
                }

                HStack(spacing: 18) {
                    Spacer()
                    Button("Cancel") {
                        dismiss()
                    }
                    .buttonStyle(TeaActionButtonStyle())
                    .focusEffectDisabled()
                    .disabled(isSaving)

                    Button {
                        save()
                    } label: {
                        if isSaving {
                            HStack(spacing: 10) {
                                ProgressView()
                                Text("Saving…")
                            }
                        } else {
                            Label("Save", systemImage: "checkmark")
                        }
                    }
                    .buttonStyle(TeaActionButtonStyle(prominent: true))
                    .focusEffectDisabled()
                    .disabled(isSaving || draftPrompt == originalPrompt)
                }
            }
            .padding(64)
            .frame(maxWidth: 1_080)
        }
        .interactiveDismissDisabled(isSaving)
    }

    private func save() {
        guard !isSaving, draftPrompt != originalPrompt else { return }
        isSaving = true
        errorMessage = nil
        Task {
            do {
                try await model.updateRecommendationPrompt(draftPrompt)
                dismiss()
            } catch {
                errorMessage = error.localizedDescription
                isSaving = false
            }
        }
    }
}
