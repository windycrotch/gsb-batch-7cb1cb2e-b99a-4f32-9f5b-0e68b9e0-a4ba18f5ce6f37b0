#include "backend.h"
#include "modem.h"

#include <array>
#include <cctype>
#include <sstream>

#include "ambivox/engine.h"

namespace ambivox {

namespace {

constexpr int kSynthesisRate = 22050;
constexpr std::array<const char *, 4> kLanguages = {"en", "fr", "es", "it"};

constexpr std::array<std::array<const char *, 4>, 12> kLexicon = {{
    {"hello", "bonjour", "hola", "ciao"},
    {"world", "monde", "mundo", "mondo"},
    {"thanks", "merci", "gracias", "grazie"},
    {"friend", "ami", "amigo", "amico"},
    {"water", "eau", "agua", "acqua"},
    {"house", "maison", "casa", "casa"},
    {"night", "nuit", "noche", "notte"},
    {"sea", "mer", "mar", "mare"},
    {"voice", "voix", "voz", "voce"},
    {"two", "deux", "dos", "due"},
    {"and", "et", "y", "e"},
    {"yes", "oui", "sí", "sì"},
}};

int language_index(const std::string & lang) {
    for (size_t i = 0; i < kLanguages.size(); ++i) {
        if (lang == kLanguages[i]) return (int) i;
    }
    throw Unsupported("unsupported language: " + lang);
}

std::string translate_word(const std::string & word, int source, int target) {
    size_t end = word.size();
    while (end > 0 && std::ispunct((unsigned char) word[end - 1])) --end;
    std::string stem = word.substr(0, end);
    for (char & c : stem) c = (char) std::tolower((unsigned char) c);
    for (const auto & row : kLexicon) {
        if (stem == row[source]) return row[target] + word.substr(end);
    }
    return word;
}

class SimBackend final : public Backend {
public:
    const char * name() const override { return "sim"; }

    std::string transcribe(const float * pcm, size_t n, const std::string & lang) override {
        language_index(lang);
        return modem::decode(pcm, n, AMBIVOX_ASR_SAMPLE_RATE);
    }

    std::string translate(const std::string & text, const std::string & source,
                          const std::string & target) override {
        return translate_stream(text, source, target, {});
    }

    std::string translate_stream(const std::string & text, const std::string & source,
                                 const std::string & target, const TextFn & on_text) override {
        const int from = language_index(source);
        const int to = language_index(target);
        if (from == to) return text;
        std::istringstream in(text);
        std::string word, out;
        while (in >> word) {
            if (!out.empty()) out += ' ';
            out += translate_word(word, from, to);
            if (on_text) on_text(out);
        }
        return out;
    }

    Audio synthesize(const std::string & text, const std::string & lang) override {
        language_index(lang);
        return {modem::encode(text, kSynthesisRate), kSynthesisRate};
    }
};

}

std::unique_ptr<Backend> make_sim_backend() { return std::make_unique<SimBackend>(); }

}
