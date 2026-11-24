#pragma once

#include <cstddef>
#include <string>
#include <vector>

namespace ambivox::modem {

inline constexpr int kTones = 17;
inline constexpr double kBaseHz = 700.0;
inline constexpr double kStepHz = 140.0;
inline constexpr double kSymbolSeconds = 0.060;
inline constexpr size_t kMaxFrameBytes = 200;

std::vector<float> encode(const std::string & text, int sample_rate);
std::string decode(const float * pcm, size_t n, int sample_rate);

}
