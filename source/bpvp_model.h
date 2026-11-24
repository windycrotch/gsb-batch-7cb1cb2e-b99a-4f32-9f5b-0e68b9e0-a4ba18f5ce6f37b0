#pragma once

#include <cstddef>

// Constants of the Basic Pitch model (nmp.tflite, Spotify, v0.4.0). They are
// shared by the plugin and by the model output conversion, and they live in a
// dedicated header without any dependency on TensorFlow Lite, on the Vamp
// plugin SDK or on the Ircam Vamp Extension so that the conversion can be
// compiled and tested on its own.
namespace Bpvp
{
    static auto constexpr modelSampleRate = 22050;
    static auto constexpr modelFFTHope = 256;
    static auto constexpr modelBlockSize = 43844;
    static auto constexpr modelBlockPadding = 7680;
    static auto constexpr modelBlockDuration = static_cast<double>(modelBlockSize - modelBlockPadding) / static_cast<double>(modelSampleRate);
    static auto constexpr modelNumFrames = 172;
    static auto constexpr modelNumNotes = 88;
    static auto constexpr modelNoteOffset = 21;
    static auto constexpr modelTensorSize = modelNumFrames * modelNumNotes;
    static auto constexpr modelBlockHop = modelBlockSize - modelBlockPadding;
    static auto constexpr modelNumOverlappingFrames = modelBlockPadding / modelFFTHope;
    static auto constexpr modelNumValidFrames = modelNumFrames - modelNumOverlappingFrames;
} // namespace Bpvp