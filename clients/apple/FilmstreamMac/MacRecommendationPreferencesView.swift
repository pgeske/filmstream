import SwiftUI

struct MacRecommendationPreferencesView: View {
    @Environment(MacAppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var draftPrompt: String
    @State private var isSaving = false
    @State private var errorMessage: String?

    private let originalPrompt: String

    init(prompt: String) {
        originalPrompt = prompt
        _draftPrompt = State(initialValue: prompt)
    }

    var body: some View {
        ZStack {
            MacTeaBackground()

            VStack(alignment: .leading, spacing: 18) {
                VStack(alignment: .leading, spacing: 6) {
                    Text("Your Taste")
                        .font(.title2.weight(.bold))
                        .foregroundStyle(Color.macTeaCream)
                    Text("Describe favorite genres and shows, the pacing you enjoy, and anything TeaStream should avoid.")
                        .foregroundStyle(Color.macTeaMuted)
                    Text("Example: Hopeful science fiction, clever mysteries, and slow-burn dramas. Avoid gore and reality TV.")
                        .font(.caption)
                        .foregroundStyle(Color.macTeaAccentLight)
                }

                TextEditor(text: $draftPrompt)
                    .font(.body)
                    .foregroundStyle(Color.macTeaCream)
                    .scrollContentBackground(.hidden)
                    .padding(10)
                    .frame(minHeight: 170)
                    .background(
                        Color.macTeaPanel.opacity(0.9),
                        in: RoundedRectangle(cornerRadius: 12, style: .continuous)
                    )
                    .overlay {
                        RoundedRectangle(cornerRadius: 12, style: .continuous)
                            .stroke(Color.macTeaCream.opacity(0.14), lineWidth: 1)
                    }
                    .accessibilityLabel("Taste description")

                Text("Leave this blank to reset your preferences.")
                    .font(.caption)
                    .foregroundStyle(Color.macTeaMuted)

                if let errorMessage {
                    Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(Color.macTeaAmber)
                }

                HStack {
                    Spacer()
                    Button("Cancel") {
                        dismiss()
                    }
                    .keyboardShortcut(.cancelAction)
                    .disabled(isSaving)

                    Button {
                        save()
                    } label: {
                        if isSaving {
                            HStack(spacing: 7) {
                                ProgressView()
                                    .controlSize(.small)
                                Text("Saving…")
                            }
                        } else {
                            Text("Save")
                        }
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(isSaving || draftPrompt == originalPrompt)
                }
            }
            .padding(28)
        }
        .frame(width: 560, height: 420)
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
