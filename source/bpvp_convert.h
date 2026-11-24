// This file contains code ported from the Basic Pitch Python library
// (https://github.com/spotify/basic-pitch, v0.4.0, basic_pitch/note_creation.py
// and basic_pitch/inference.py):
//
// Copyright 2022 Spotify AB
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Modified by Pierre Guillot (IRCAM): translated from Python to C++ and adapted
// to the Basic Pitch Vamp Plugin.

#pragma once

#include "bpvp_model.h"

#include <array>
#include <cmath>
#include <functional>
#include <optional>
#include <vector>

namespace Bpvp
{
    struct Note
    {
        double start;
        double end;
        float pitch;
        float amplitude;
    };

    // Returns the time in seconds of a frame of the unwrapped model output
    // (each model block contributes modelNumValidFrames frames and the blocks are spaced by modelBlockHop samples)
    double frameToSeconds(size_t frame);

    // Port of basic_pitch.note_creation.output_to_notes_polyphonic, the overlapping notes with the same pitch are merged
    // and the notes are split into monophonic voices preserving the pitch order (when two notes overlap, the lower note is
    // in a lower voice, 0 being the lowest). The function returns the notes of the maxNumVoices voices (the voices of the
    // model, the highest ones are usually empty). The voice of a note only depends on the notes of lower pitches, so the
    // assignment does not depend on any voice limit and the notes of a voice are sorted by start time.
    std::vector<std::vector<Note>> getVoices(std::vector<std::array<float, modelNumNotes>> const& frames, std::vector<std::array<float, modelNumNotes>> const& onsets, bool inferOnsets, float frameEnergyThreshold, float onsetEnergyThreshold, double minNoteDuration, long maxFramesBelowThreshold, std::optional<float> const minFreq = {}, std::optional<float> const maxFreq = {}, bool melodiaTrick = true);

    // Returns the first maxVoices voices of a voice assignment (the missing voices are empty). The projection only
    // selects the voices, it does not modify the notes, so the output of a limit is always the prefix of the output of
    // a higher limit.
    std::vector<std::vector<Note>> projectVoices(std::vector<std::vector<Note>> const& voices, size_t maxVoices);

    // Port of basic_pitch.note_creation.output_to_notes_polyphonic limited to the first maxVoices voices. The notes
    // (start, end, pitch and amplitude) do not depend on maxVoices: the result is exactly the projection of getVoices(),
    // so changing the number of voices never changes, removes or adds a note in the retained voices.
    std::vector<std::vector<Note>> getNotes(std::vector<std::array<float, modelNumNotes>> const& frames, std::vector<std::array<float, modelNumNotes>> const& onsets, bool inferOnsets, size_t maxVoices, float frameEnergyThreshold, float onsetEnergyThreshold, double minNoteDuration, long maxFramesBelowThreshold, std::optional<float> const minFreq = {}, std::optional<float> const maxFreq = {}, bool melodiaTrick = true);
} // namespace Bpvp
