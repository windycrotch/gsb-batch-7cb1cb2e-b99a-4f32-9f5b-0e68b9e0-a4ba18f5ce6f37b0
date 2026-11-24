// Regression test of the model output conversion (voices and notes).
//
// It checks the properties that the number of voices (the limit on the
// outputs) must not change:
//
// - the notes are assigned to the voices independently of the limit, so the
//   notes of the first N voices are the same whatever the limit, and the notes
//   of a higher limit are a superset of the notes of a lower limit;
// - the notes of a voice are sorted by start time and the voices are ordered by
//   pitch (a note of a lower voice never has a higher pitch than an overlapping
//   note of a higher voice);
// - the start, end, pitch and amplitude of a note do not depend on the limit;
// - a limit of one voice only returns the lowest voice, a limit of maxNumVoices
//   returns every voice, and the frame and onset thresholds and the minimum
//   note duration are still applied.
//
// The test only depends on the conversion (bpvp_convert) and the model
// constants (bpvp_model), not on TensorFlow Lite nor on the Vamp plugin SDK.

#include "bpvp_convert.h"
#include "bpvp_model.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdio>
#include <random>
#include <string>
#include <vector>

namespace
{
    using Matrix = std::vector<std::array<float, Bpvp::modelNumNotes>>;
    using Notes = std::vector<std::vector<Bpvp::Note>>;

    // The maximum number of voices of the plugin (the conversion has no limit of its own)
    size_t constexpr maxVoices = 24;

    int failures = 0;

    void expect(bool condition, std::string const& message)
    {
        if(!condition)
        {
            std::printf("FAIL: %s\n", message.c_str());
            ++failures;
        }
    }

    // Adds a sustained note to the frames (energy) and an onset at its first frame
    void addNote(Matrix& frames, Matrix& onsets, int midi, size_t start, size_t length, float amplitude)
    {
        auto const note = static_cast<size_t>(midi - Bpvp::modelNoteOffset);
        if(note >= Bpvp::modelNumNotes || start >= frames.size())
        {
            return;
        }
        for(auto frame = start; frame < std::min(frames.size(), start + length); ++frame)
        {
            frames[frame][note] = std::max(frames[frame][note], amplitude);
        }
        onsets[start][note] = std::max(onsets[start][note], amplitude);
    }

    bool sameNote(Bpvp::Note const& lhs, Bpvp::Note const& rhs)
    {
        return lhs.start == rhs.start && lhs.end == rhs.end && lhs.pitch == rhs.pitch && lhs.amplitude == rhs.amplitude;
    }

    void checkLimitIndependence(Matrix const& frames, Matrix const& onsets, std::string const& name)
    {
        auto const convert = [&](size_t voices)
        {
            return Bpvp::getNotes(frames, onsets, true, voices, 0.3f, 0.5f, 0.128, 11, 80.0f, 8000.0f);
        };
        auto const reference = convert(maxVoices);

        for(size_t limit = 1; limit <= maxVoices; ++limit)
        {
            auto const notes = convert(limit);
            expect(notes.size() == limit, name + ": the number of outputs is the limit (" + std::to_string(limit) + ")");
            for(size_t voice = 0; voice < std::min(limit, reference.size()); ++voice)
            {
                expect(notes[voice].size() == reference[voice].size(), name + ": voice " + std::to_string(voice) + " has the same number of notes for every limit");
                for(size_t i = 0; i < std::min(notes[voice].size(), reference[voice].size()); ++i)
                {
                    expect(sameNote(notes[voice][i], reference[voice][i]), name + ": note " + std::to_string(i) + " of voice " + std::to_string(voice) + " does not depend on the limit");
                }
                // The notes of a voice are sorted by start
                expect(std::is_sorted(notes[voice].cbegin(), notes[voice].cend(), [](auto const& lhs, auto const& rhs)
                                      {
                                          return lhs.start < rhs.start;
                                      }),
                       name + ": the notes of voice " + std::to_string(voice) + " are sorted by start");
            }
            // The outputs of a limit are a prefix of the outputs of a higher limit (already covered by the loop above), and
            // a lower voice never carries a note above the pitch of an overlapping note of a higher voice
            for(size_t voice = 0; voice + 1 < notes.size(); ++voice)
            {
                for(auto const& low : notes[voice])
                {
                    for(auto const& high : notes[voice + 1])
                    {
                        auto const overlap = low.start < high.end && high.start < low.end;
                        expect(!overlap || low.pitch <= high.pitch, name + ": the voices are ordered by pitch");
                    }
                }
            }
        }

        // The notes that a lower limit drops must not appear in any output of that limit: the
        // output of a limit of one voice is exactly the lowest voice of the full assignment.
        auto const one = convert(1);
        auto const all = convert(maxVoices);
        size_t kept = 0;
        for(auto const& notes : one)
        {
            kept += notes.size();
        }
        expect(kept == all[0].size(), name + ": a limit of one voice returns only the lowest voice");
        expect(one[0].size() == all[0].size(), name + ": a limit of one voice returns the lowest voice unchanged");
        for(size_t i = 0; i < std::min(one[0].size(), all[0].size()); ++i)
        {
            expect(sameNote(one[0][i], all[0][i]), name + ": a limit of one voice keeps the lowest voice notes unchanged");
        }
    }
} // namespace

int main()
{
    // Four simultaneous voices held together, then a higher melody, plus an inner voice: a small polyphonic texture
    {
        Matrix frames(400), onsets(400);
        addNote(frames, onsets, 36, 0, 300, 0.90f);
        addNote(frames, onsets, 48, 0, 300, 0.80f);
        addNote(frames, onsets, 55, 0, 300, 0.70f);
        addNote(frames, onsets, 72, 0, 150, 0.95f);
        addNote(frames, onsets, 71, 200, 100, 0.85f);
        addNote(frames, onsets, 43, 100, 200, 0.60f);
        checkLimitIndependence(frames, onsets, "texture");
    }

    // A dense stack of simultaneous notes, so that many voices are used
    {
        Matrix frames(300), onsets(300);
        int const pitches[] = {40, 47, 52, 59, 64, 67, 72, 76};
        auto amplitude = 0.95f;
        for(auto const pitch : pitches)
        {
            addNote(frames, onsets, pitch, 20, 250, amplitude);
            amplitude -= 0.05f;
        }
        checkLimitIndependence(frames, onsets, "stack");
    }

    // Randomized model outputs (deterministic seed) to cover uneven textures and empty voices
    {
        std::mt19937 rng(20241124);
        for(int trial = 0; trial < 40; ++trial)
        {
            auto const numFrames = static_cast<size_t>(60 + rng() % 240);
            Matrix frames(numFrames), onsets(numFrames);
            auto const numNotes = 1 + static_cast<int>(rng() % 10);
            for(int i = 0; i < numNotes; ++i)
            {
                auto const midi = static_cast<int>(rng() % Bpvp::modelNumNotes) + Bpvp::modelNoteOffset;
                auto const start = static_cast<size_t>(rng() % numFrames);
                auto const length = static_cast<size_t>(1 + rng() % (numFrames - start));
                auto const amplitude = 0.2f + 0.8f * static_cast<float>(rng() % 1000) / 1000.0f;
                addNote(frames, onsets, midi, start, length, amplitude);
            }
            checkLimitIndependence(frames, onsets, "random " + std::to_string(trial));
        }
    }

    // The thresholds and the minimum note duration keep their semantics: a note
    // below the onset threshold is not detected, and a note shorter than the
    // minimum duration is discarded
    {
        Matrix frames(200), onsets(200);
        addNote(frames, onsets, 60, 10, 100, 0.30f); // below the onset threshold (0.5)
        auto low = Bpvp::getNotes(frames, onsets, true, 4, 0.3f, 0.5f, 0.128, 11, 80.0f, 8000.0f);
        size_t lowCount = 0;
        for(auto const& notes : low)
        {
            lowCount += notes.size();
        }
        expect(lowCount == 0, "thresholds: a note below the onset threshold is not detected");

        Matrix shortFrames(200), shortOnsets(200);
        addNote(shortFrames, shortOnsets, 60, 10, 5, 0.9f); // shorter than the minimum duration
        auto shortNotes = Bpvp::getNotes(shortFrames, shortOnsets, true, 4, 0.3f, 0.5f, 0.128, 11, 80.0f, 8000.0f);
        size_t shortCount = 0;
        for(auto const& notes : shortNotes)
        {
            shortCount += notes.size();
        }
        expect(shortCount == 0, "thresholds: a note shorter than the minimum duration is discarded");
    }

    if(failures == 0)
    {
        std::printf("All voice-limit checks passed.\n");
        return 0;
    }
    std::printf("%d check(s) failed.\n", failures);
    return 1;
}
