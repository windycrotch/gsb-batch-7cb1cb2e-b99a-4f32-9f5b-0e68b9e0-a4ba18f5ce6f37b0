#include "modem.h"

#include <algorithm>
#include <cmath>
#include <cstdint>

namespace ambivox::modem {

namespace {

constexpr double kPi = 3.14159265358979323846;
constexpr double kAmplitude = 0.5;
constexpr double kRampSeconds = 0.005;
constexpr double kLeadSeconds = 0.040;
constexpr double kGapSeconds = 0.160;
constexpr double kHopSeconds = 0.010;
constexpr double kWindowSeconds = 0.020;
constexpr int kMinSymbolHops = 3;
constexpr int kFrameEndHops = 8;
constexpr double kMinPower = 1e-5;
constexpr double kMinPurity = 0.4;

uint8_t crc8(const uint8_t * data, size_t n) {
    uint8_t crc = 0;
    for (size_t i = 0; i < n; ++i) {
        crc ^= data[i];
        for (int b = 0; b < 8; ++b) crc = (crc & 0x80) ? (uint8_t) ((crc << 1) ^ 0x07) : (uint8_t) (crc << 1);
    }
    return crc;
}

double tone_hz(int k) { return kBaseHz + kStepHz * k; }

void append_silence(std::vector<float> & out, double seconds, int sample_rate) {
    out.insert(out.end(), (size_t) std::lround(seconds * sample_rate), 0.0f);
}

void append_tone(std::vector<float> & out, int k, int sample_rate) {
    const size_t n = (size_t) std::lround(kSymbolSeconds * sample_rate);
    const size_t ramp = (size_t) std::lround(kRampSeconds * sample_rate);
    const double w = 2.0 * kPi * tone_hz(k) / sample_rate;
    for (size_t i = 0; i < n; ++i) {
        double gain = 1.0;
        if (i < ramp) gain = 0.5 - 0.5 * std::cos(kPi * i / ramp);
        else if (i >= n - ramp) gain = 0.5 - 0.5 * std::cos(kPi * (n - 1 - i) / ramp);
        out.push_back((float) (kAmplitude * gain * std::sin(w * i)));
    }
}

void append_frame(std::vector<float> & out, const uint8_t * payload, size_t len, int sample_rate) {
    std::vector<uint8_t> bytes;
    bytes.reserve(len + 2);
    bytes.push_back((uint8_t) len);
    bytes.insert(bytes.end(), payload, payload + len);
    bytes.push_back(crc8(bytes.data(), bytes.size()));

    int tone = 0;
    append_tone(out, tone, sample_rate);
    for (uint8_t b : bytes) {
        for (int nibble : {b >> 4, b & 0x0f}) {
            tone = (tone + nibble + 1) % kTones;
            append_tone(out, tone, sample_rate);
        }
    }
}

int classify(const float * x, size_t n, int sample_rate) {
    double energy = 0.0;
    for (size_t i = 0; i < n; ++i) energy += (double) x[i] * x[i];
    if (energy / n < kMinPower) return -1;

    int best = -1;
    double best_power = 0.0;
    for (int k = 0; k < kTones; ++k) {
        const double coeff = 2.0 * std::cos(2.0 * kPi * tone_hz(k) / sample_rate);
        double s1 = 0.0, s2 = 0.0;
        for (size_t i = 0; i < n; ++i) {
            const double s0 = x[i] + coeff * s1 - s2;
            s2 = s1;
            s1 = s0;
        }
        const double power = s1 * s1 + s2 * s2 - coeff * s1 * s2;
        if (power > best_power) {
            best_power = power;
            best = k;
        }
    }
    return best_power / (energy * n / 2.0) >= kMinPurity ? best : -1;
}

bool parse_frame(const std::vector<int> & tones, std::string & out) {
    if (tones.size() < 5 || (tones.size() - 1) % 2 != 0) return false;
    std::vector<uint8_t> bytes;
    for (size_t i = 1; i + 1 < tones.size(); i += 2) {
        const int hi = (tones[i] - tones[i - 1] - 1 + kTones) % kTones;
        const int lo = (tones[i + 1] - tones[i] - 1 + kTones) % kTones;
        if (hi > 15 || lo > 15) return false;
        bytes.push_back((uint8_t) ((hi << 4) | lo));
    }
    if (bytes.size() != (size_t) bytes[0] + 2) return false;
    if (crc8(bytes.data(), bytes.size() - 1) != bytes.back()) return false;
    out.append(bytes.begin() + 1, bytes.end() - 1);
    return true;
}

}

std::vector<float> encode(const std::string & text, int sample_rate) {
    std::vector<float> out;
    append_silence(out, kLeadSeconds, sample_rate);
    const uint8_t * data = (const uint8_t *) text.data();
    size_t offset = 0;
    do {
        size_t end = text.find(' ', offset);
        end = end == std::string::npos ? text.size() : text.find_first_not_of(' ', end);
        if (end == std::string::npos) end = text.size();
        const size_t len = std::min(kMaxFrameBytes, end - offset);
        if (offset > 0) append_silence(out, kGapSeconds, sample_rate);
        append_frame(out, data + offset, len, sample_rate);
        offset += len;
    } while (offset < text.size());
    append_silence(out, kLeadSeconds, sample_rate);
    return out;
}

std::string decode(const float * pcm, size_t n, int sample_rate) {
    const size_t hop = (size_t) std::lround(kHopSeconds * sample_rate);
    const size_t window = (size_t) std::lround(kWindowSeconds * sample_rate);

    std::string text;
    std::vector<int> tones;
    int current = -1, run = 0, silence = 0;
    bool counted = false;

    auto flush = [&] {
        if (!tones.empty()) parse_frame(tones, text);
        tones.clear();
    };

    for (size_t pos = 0; pos + window <= n; pos += hop) {
        const int k = classify(pcm + pos, window, sample_rate);
        if (k == current) {
            ++run;
        } else {
            current = k;
            run = 1;
            counted = false;
        }
        if (k < 0) {
            if (++silence >= kFrameEndHops) flush();
            continue;
        }
        silence = 0;
        if (!counted && run >= kMinSymbolHops) {
            if (tones.empty() || tones.back() != k) tones.push_back(k);
            counted = true;
        }
    }
    flush();
    return text;
}

}
