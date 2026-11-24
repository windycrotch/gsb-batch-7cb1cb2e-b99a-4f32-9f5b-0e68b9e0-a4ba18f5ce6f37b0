#pragma once

#include <IvePluginAdapter.hpp>
#include <array>
#include <memory>
#include <set>
#include <tensorflow/lite/c/c_api.h>
#include <vector>

#include "bpvp_model.h"

namespace Bpvp
{
    class Plugin
    : public Vamp::Plugin
    , public Ive::PluginExtension
    {
    public:
        Plugin(float inputSampleRate);
        ~Plugin() override = default;

        // Vamp::Plugin
        bool initialise(size_t channels, size_t stepSize, size_t blockSize) override;

        InputDomain getInputDomain() const override;

        std::string getIdentifier() const override;
        std::string getName() const override;
        std::string getDescription() const override;
        std::string getMaker() const override;
        int getPluginVersion() const override;
        std::string getCopyright() const override;

        size_t getPreferredBlockSize() const override;
        size_t getPreferredStepSize() const override;
        OutputList getOutputDescriptors() const override;

        void reset() override;
        FeatureSet process(float const* const* inputBuffers, Vamp::RealTime timestamp) override;
        FeatureSet getRemainingFeatures() override;

        ParameterList getParameterDescriptors() const override;
        float getParameter(std::string paramid) const override;
        void setParameter(std::string paramid, float newval) override;

        // Ive::PluginExtension
        OutputExtraList getOutputExtraDescriptors(size_t outputDescriptorIndex) const override;
        std::optional<VoiceDescriptor> getOutputVoiceDescriptor(size_t outputDescriptorIndex) const override;
        size_t getOutputVoiceCount(size_t outputDescriptorIndex) const override;
        void setOutputVoiceCount(std::string const& identifier, size_t count) override;

    private:
        using Matrix = std::vector<std::array<float, modelNumNotes>>;

        // Runs the model on the windows of the streaming buffer (the remaining samples are padded with zeros when
        // flushing) and appends the valid frames of the model to the accumulated matrices. The audio buffer and its
        // position are consumed, so the analysis is only flushed once (see getRemainingFeatures).
        void processModel(bool flush);

        class Resampler
        {
        public:
            Resampler() = default;
            ~Resampler() = default;

            void prepare(double sampleRate);
            std::tuple<size_t, size_t> process(size_t numInputSamples, float const* inputBuffer, size_t numOutputSamples, float* outputBuffer);
            void reset();

            void setTargetSampleRate(double sampleRate) noexcept;
            double getRatio() const noexcept;

        private:
            double mSourceSampleRate{48000.0};
            double mTargetSampleRate{static_cast<double>(modelSampleRate)};
            std::array<float, 5> mLastInputSamples;
            double mSubSamplePos{1.0};
            size_t mIndexBuffer{0};
        };

        using model_uptr = std::unique_ptr<TfLiteModel, void (*)(TfLiteModel*)>;
        using interpreter_options_uptr = std::unique_ptr<TfLiteInterpreterOptions, void (*)(TfLiteInterpreterOptions*)>;
        using interpreter_uptr = std::unique_ptr<TfLiteInterpreter, void (*)(TfLiteInterpreter*)>;
        model_uptr mModel{nullptr, nullptr};
        interpreter_uptr mInterpreter{nullptr, nullptr};
        Resampler mResampler;
        std::array<float, modelBlockSize * 2> mInputBuffer;
        std::array<float, modelBlockSize> mAudioBuffer;
        std::array<float, modelTensorSize> mOuputBuffer;
        Matrix mAccumulatedFrames;
        Matrix mAccumulatedOnsets;
        // Whether the accumulated frames already contain the final (zero-padded) windows of the model. It prevents the
        // finalisation from running twice, so getRemainingFeatures can be called again (e.g. after the number of voices
        // was changed) without appending the padded windows to the analysis again.
        bool mFinalised{false};
        size_t mInputBufferPosition{0};
        size_t mNumSamples{0};
        Vamp::RealTime mStartTime;
        size_t mBlockSize{0};
        static size_t constexpr maxNumVoices = 24;
        static size_t constexpr defaultNumVoices = 4;
        // The maximum number of voices is a setting of the analysis, not a state of the accumulated analysis: it is
        // kept by reset() and the notes are assigned to the voices independently of it (see getVoices), so the limit
        // can be changed at any time and the retained notes never change.
        size_t mNumVoices{defaultNumVoices};
        float mFrameThreshold{0.3f};
        float mOnsetThreshold{0.5f};
        int mMinNoteDuration{128};
    };
} // namespace Bpvp
