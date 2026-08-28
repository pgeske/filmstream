import SwiftUI

struct IOSRecommendationPreferencesView: View {
    @Environment(IOSAppModel.self) private var model
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
        NavigationStack {
            Form {
                Section {
                    Text("Describe favorite genres and shows, the pacing you enjoy, and anything you want TeaStream to avoid.")
                        .foregroundStyle(Color.mobileTeaMuted)
                    Text("For example: “Hopeful science fiction, clever mysteries, and slow-burn dramas. Avoid gore and reality TV.”")
                        .font(.footnote)
                        .foregroundStyle(Color.mobileTeaAccentLight)
                }

                Section("Your taste") {
                    TextEditor(text: $draftPrompt)
                        .frame(minHeight: 180)
                        .accessibilityLabel("Taste description")
                    Text("Leave this blank to reset your preferences.")
                        .font(.caption)
                        .foregroundStyle(Color.mobileTeaMuted)
                }

                if let errorMessage {
                    Section {
                        Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                            .font(.footnote.weight(.semibold))
                            .foregroundStyle(Color.mobileTeaAmber)
                    }
                }
            }
            .scrollContentBackground(.hidden)
            .background(MobileTeaBackground())
            .navigationTitle("Your Taste")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        dismiss()
                    }
                    .disabled(isSaving)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button {
                        save()
                    } label: {
                        if isSaving {
                            ProgressView()
                        } else {
                            Text("Save")
                                .fontWeight(.semibold)
                        }
                    }
                    .disabled(isSaving || draftPrompt == originalPrompt)
                }
            }
        }
        .tint(Color.mobileTeaAccent)
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
