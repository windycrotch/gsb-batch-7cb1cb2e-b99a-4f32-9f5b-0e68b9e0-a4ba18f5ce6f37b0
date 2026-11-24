#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <random>
#include <string>
#include <vector>

#include "ambivox/engine.h"
#include "modem.h"

namespace {

int failures = 0;

#define CHECK(cond)                                                        \
    do {                                                                   \
        if (!(cond)) {                                                     \
            std::fprintf(stderr, "%s:%d: CHECK(%s) failed\n", __FILE__, __LINE__, #cond); \
            ++failures;                                                    \
        }                                                                  \
    } while (0)

std::vector<float> resample_linear(const std::vector<float> & in, int from, int to) {
    std::vector<float> out((size_t) ((double) in.size() * to / from));
    for (size_t i = 0; i < out.size(); ++i) {
        const double pos = (double) i * from / to;
        const size_t a = (size_t) pos;
        const size_t b = a + 1 < in.size() ? a + 1 : a;
        const double frac = pos - a;
        out[i] = (float) (in[a] * (1.0 - frac) + in[b] * frac);
    }
    return out;
}

void test_modem_roundtrip() {
    for (const char * text : {"a", "hello world", "voix à voix, sì!", "  spaced   out ", ""}) {
        for (int rate : {16000, 22050, 48000}) {
            const auto pcm = ambivox::modem::encode(text, rate);
            CHECK(ambivox::modem::decode(pcm.data(), pcm.size(), rate) == text);
        }
    }
}

void test_modem_repeated_bytes() {
    const std::string text(40, 'f');
    const auto pcm = ambivox::modem::encode(text, 16000);
    CHECK(ambivox::modem::decode(pcm.data(), pcm.size(), 16000) == text);
}

void test_modem_multi_frame() {
    std::string text;
    for (int i = 0; i < 500; ++i) text += (char) ('a' + i % 26);
    const auto pcm = ambivox::modem::encode(text, 16000);
    CHECK(ambivox::modem::decode(pcm.data(), pcm.size(), 16000) == text);
}

void test_modem_reveals_words_as_they_arrive() {
    const std::string text = "thanks my friend";
    const auto pcm = ambivox::modem::encode(text, 16000);
    std::vector<std::string> seen;
    for (size_t n = 0; n <= pcm.size(); n += 1600) {
        const std::string heard = ambivox::modem::decode(pcm.data(), n, 16000);
        CHECK(text.compare(0, heard.size(), heard) == 0);
        if (seen.empty() || seen.back() != heard) seen.push_back(heard);
    }
    const std::vector<std::string> expected = {"", "thanks ", "thanks my ", "thanks my friend"};
    CHECK(seen == expected);
}

void test_modem_survives_noise_gain_and_resampling() {
    const std::string text = "hello friend";
    auto pcm = resample_linear(ambivox::modem::encode(text, 22050), 22050, 16000);
    std::mt19937 rng(7);
    std::normal_distribution<float> noise(0.0f, 0.01f);
    for (float & s : pcm) s = s * 0.2f + noise(rng);
    CHECK(ambivox::modem::decode(pcm.data(), pcm.size(), 16000) == text);
}

void test_modem_rejects_garbage() {
    std::vector<float> silence(16000, 0.0f);
    CHECK(ambivox::modem::decode(silence.data(), silence.size(), 16000).empty());

    std::mt19937 rng(11);
    std::uniform_real_distribution<float> uniform(-0.5f, 0.5f);
    std::vector<float> noise(32000);
    for (float & s : noise) s = uniform(rng);
    CHECK(ambivox::modem::decode(noise.data(), noise.size(), 16000).empty());

    auto pcm = ambivox::modem::encode("helloworld", 16000);
    pcm.resize(pcm.size() / 2);
    CHECK(ambivox::modem::decode(pcm.data(), pcm.size(), 16000).empty());
}

void test_engine_pipeline() {
    ambivox_config config = {"sim", nullptr, 0};
    ambivox_engine * engine = ambivox_engine_open(&config);
    CHECK(engine != nullptr);
    if (engine == nullptr) return;
    CHECK(std::strcmp(ambivox_engine_backend(engine), "sim") == 0);

    float * pcm = nullptr;
    size_t n = 0;
    int rate = 0;
    CHECK(ambivox_synthesize(engine, "Hello, world", "en", &pcm, &n, &rate) == AMBIVOX_OK);
    CHECK(n > 0 && rate == 22050);

    const std::vector<float> heard = resample_linear(std::vector<float>(pcm, pcm + n), rate, AMBIVOX_ASR_SAMPLE_RATE);
    ambivox_free(pcm);

    char * text = nullptr;
    CHECK(ambivox_transcribe(engine, heard.data(), heard.size(), "en", &text) == AMBIVOX_OK);
    CHECK(text != nullptr && std::strcmp(text, "Hello, world") == 0);

    char * translated = nullptr;
    CHECK(ambivox_translate(engine, text, "en", "fr", &translated) == AMBIVOX_OK);
    CHECK(translated != nullptr && std::strcmp(translated, "bonjour, monde") == 0);
    ambivox_free(translated);

    CHECK(ambivox_translate(engine, text, "en", "en", &translated) == AMBIVOX_OK);
    CHECK(std::strcmp(translated, text) == 0);
    ambivox_free(translated);
    ambivox_free(text);

    CHECK(ambivox_translate(engine, "hola amigo", "es", "it", &translated) == AMBIVOX_OK);
    CHECK(std::strcmp(translated, "ciao amico") == 0);
    ambivox_free(translated);

    std::vector<std::string> grown;
    const ambivox_text_fn collect = [](void * user, const char * so_far) {
        static_cast<std::vector<std::string> *>(user)->push_back(so_far);
    };
    CHECK(ambivox_translate_stream(engine, "hola amigo", "es", "it", collect, &grown, &translated) == AMBIVOX_OK);
    CHECK(std::strcmp(translated, "ciao amico") == 0);
    CHECK(grown == std::vector<std::string>({"ciao", "ciao amico"}));
    ambivox_free(translated);
    CHECK(ambivox_translate_stream(engine, "hola amigo", "es", "it", nullptr, nullptr, &translated) == AMBIVOX_OK);
    CHECK(std::strcmp(translated, "ciao amico") == 0);
    ambivox_free(translated);

    ambivox_engine_close(engine);
}

void test_engine_errors() {
    ambivox_config config = {"sim", nullptr, 0};
    ambivox_engine * engine = ambivox_engine_open(&config);
    char * out = nullptr;

    CHECK(ambivox_translate(engine, "hello", "en", "xx", &out) == AMBIVOX_ERR_UNSUPPORTED);
    CHECK(out == nullptr);
    CHECK(std::strstr(ambivox_last_error(), "xx") != nullptr);

    CHECK(ambivox_translate(engine, nullptr, "en", "fr", &out) == AMBIVOX_ERR_ARGUMENT);
    CHECK(ambivox_transcribe(nullptr, nullptr, 0, "en", &out) == AMBIVOX_ERR_ARGUMENT);

    CHECK(ambivox_transcribe(engine, nullptr, 0, "en", &out) == AMBIVOX_OK);
    CHECK(out != nullptr && out[0] == '\0');
    ambivox_free(out);
    ambivox_engine_close(engine);

    ambivox_config unknown = {"nope", nullptr, 0};
    CHECK(ambivox_engine_open(&unknown) == nullptr);
    CHECK(std::strstr(ambivox_last_error(), "nope") != nullptr);

    ambivox_engine * fallback = ambivox_engine_open(nullptr);
    CHECK(fallback != nullptr);
    ambivox_engine_close(fallback);
}

}

int main() {
    test_modem_roundtrip();
    test_modem_repeated_bytes();
    test_modem_multi_frame();
    test_modem_reveals_words_as_they_arrive();
    test_modem_survives_noise_gain_and_resampling();
    test_modem_rejects_garbage();
    test_engine_pipeline();
    test_engine_errors();
    if (failures != 0) {
        std::fprintf(stderr, "%d check(s) failed\n", failures);
        return 1;
    }
    std::puts("engine tests passed");
    return 0;
}
