#include "backend.h"
#include "llama_translator.h"

#include <cctype>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <map>
#include <mutex>
#include <utility>

#include "sherpa-onnx/c-api/c-api.h"
#include "sussurro.h"
#include "whisper.h"

namespace ambivox {

namespace {

namespace fs = std::filesystem;

constexpr int kMaxNewTokens = 256;
constexpr int kBeamSize = 1;
constexpr float kNoSpeech = 0.6f;
constexpr float kLowConfidence = -1.0f;

void quiet(ggml_log_level level, const char * text, void *) {
    if (level == GGML_LOG_LEVEL_ERROR) fputs(text, stderr);
}

struct Route {
    const char * source;
    const char * target;
    const char * model;
    const char * token;
};

constexpr Route kRoutes[] = {
    {"en", "fr", "tc-en-fr.gguf", ""},      {"en", "es", "tc-en-es.gguf", ""},
    {"en", "it", "tc-en-it.gguf", ""},      {"fr", "en", "tc-fr-en.gguf", ""},
    {"it", "en", "tc-it-en.gguf", ""},      {"es", "en", "es-en.gguf", ""},
    {"it", "fr", "tc-itc-itc.gguf", "fra"}, {"es", "fr", "tc-itc-itc.gguf", "fra"},
    {"fr", "it", "tc-itc-itc.gguf", "ita"}, {"es", "it", "tc-itc-itc.gguf", "ita"},
    {"fr", "es", "tc-itc-itc.gguf", "spa"}, {"it", "es", "tc-itc-itc.gguf", "spa"},
};

constexpr std::pair<const char *, const char *> kVoices[] = {
    {"en", "vits-piper-en_US-ryan-medium"},
    {"fr", "vits-piper-fr_FR-tom-medium"},
    {"es", "vits-piper-es_ES-davefx-medium"},
    {"it", "vits-piper-it_IT-paola-medium"},
};

struct Translator {
    sussurro_model model;
    sussurro_tokenizer * tokenizer = nullptr;
    std::mutex mutex;

    ~Translator() {
        if (tokenizer != nullptr) sussurro_tokenizer_free(tokenizer);
        sussurro_model_free(model);
    }
};

struct Voice {
    const SherpaOnnxOfflineTts * tts = nullptr;
    std::mutex mutex;

    ~Voice() {
        if (tts != nullptr) SherpaOnnxDestroyOfflineTts(tts);
    }
};

std::string find_onnx(const fs::path & dir) {
    std::error_code ec;
    for (const auto & entry : fs::directory_iterator(dir, ec)) {
        if (entry.path().extension() == ".onnx") return entry.path().string();
    }
    return "";
}

bool valid_model_name(const std::string & name) {
    if (name.empty() || name.front() == '.') return false;
    for (const char c : name) {
        if (!std::isalnum(static_cast<unsigned char>(c)) && c != '-' && c != '_' && c != '.') return false;
    }
    return true;
}

// Whisper writes something even for a cough or a door. Like OpenAI's reference
// decoder, a segment counts as silence when the model both expects no speech
// and has little confidence in the words it produced.
bool silent(whisper_context * ctx, int segment) {
    if (whisper_full_get_segment_no_speech_prob(ctx, segment) <= kNoSpeech) return false;
    const whisper_token eot = whisper_token_eot(ctx);
    float sum = 0.0f;
    int count = 0;
    for (int i = 0; i < whisper_full_n_tokens(ctx, segment); ++i) {
        const whisper_token_data token = whisper_full_get_token_data(ctx, segment, i);
        if (token.id >= eot) continue;
        sum += token.plog;
        ++count;
    }
    return count == 0 || sum / count < kLowConfidence;
}

std::string trim(const std::string & s) {
    const size_t first = s.find_first_not_of(" \t\r\n");
    if (first == std::string::npos) return "";
    const size_t last = s.find_last_not_of(" \t\r\n");
    return s.substr(first, last - first + 1);
}

class SussurroBackend final : public Backend {
public:
    SussurroBackend(const std::string & model_dir, int threads, const std::string & asr_model,
                    const std::string & translator)
        : dir_(model_dir),
          whisper_model_("ggml-" + asr_model + ".bin"),
          llm_model_(translator == "opus" ? "" : translator + ".gguf"),
          threads_(threads > 0 ? threads : 4) {
        if (!fs::is_directory(dir_)) throw ModelError("model directory not found: " + model_dir);
        if (!valid_model_name(asr_model)) throw ModelError("invalid whisper model name: " + asr_model);
        if (!valid_model_name(translator)) throw ModelError("invalid translation model name: " + translator);
        whisper_log_set(quiet, nullptr);
    }

    ~SussurroBackend() override {
        llm_.reset();
        translators_.clear();
        voices_.clear();
        if (whisper_ != nullptr) whisper_free(whisper_);
    }

    const char * name() const override { return "sussurro"; }

    std::string transcribe(const float * pcm, size_t n, const std::string & lang) override {
        whisper_full_params params = whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
        params.language = lang.c_str();
        params.translate = false;
        params.no_timestamps = true;
        params.no_context = true;
        params.single_segment = false;
        params.suppress_blank = true;
        params.suppress_nst = true;
        params.print_progress = false;
        params.print_realtime = false;
        params.print_special = false;
        params.print_timestamps = false;
        params.n_threads = threads_;

        std::lock_guard<std::mutex> lock(whisper_mutex_);
        if (whisper_ == nullptr) {
            const std::string path = (dir_ / whisper_model_).string();
            if (!fs::exists(path)) throw ModelError("whisper model not found: " + path);
            whisper_ = whisper_init_from_file_with_params(path.c_str(), whisper_context_default_params());
            if (whisper_ == nullptr) throw ModelError("cannot load whisper model: " + path);
        }
        if (whisper_full(whisper_, params, pcm, (int) n) != 0) throw std::runtime_error("whisper inference failed");
        std::string text;
        const int segments = whisper_full_n_segments(whisper_);
        for (int i = 0; i < segments; ++i) {
            if (!silent(whisper_, i)) text += whisper_full_get_segment_text(whisper_, i);
        }
        return trim(text);
    }

    std::string translate_stream(const std::string & text, const std::string & source,
                                 const std::string & target, const TextFn & on_text) override {
        if (llm_model_.empty() || source == target || text.empty()) return translate(text, source, target);
        return trim(llm().translate(text, source, target, on_text));
    }

    std::string translate(const std::string & text, const std::string & source,
                          const std::string & target) override {
        if (source == target || text.empty()) return text;
        if (!llm_model_.empty()) return trim(llm().translate(text, source, target));
        const Route * route = nullptr;
        for (const Route & r : kRoutes) {
            if (source == r.source && target == r.target) route = &r;
        }
        if (route == nullptr) throw Unsupported("no translation model for " + source + " -> " + target);

        Translator & translator = this->translator(route->model);
        const std::string token = route->token[0] != '\0' ? ">>" + std::string(route->token) + "<<" : "";
        std::lock_guard<std::mutex> lock(translator.mutex);
        return trim(sussurro_translate(translator.model, translator.tokenizer, text, kMaxNewTokens, kBeamSize, token));
    }

    Audio synthesize(const std::string & text, const std::string & lang) override {
        Voice & voice = this->voice(lang);
        SherpaOnnxGenerationConfig config;
        std::memset(&config, 0, sizeof(config));
        config.speed = 1.0f;
        config.sid = 0;
        config.silence_scale = 0.2f;

        std::lock_guard<std::mutex> lock(voice.mutex);
        const SherpaOnnxGeneratedAudio * generated =
            SherpaOnnxOfflineTtsGenerateWithConfig(voice.tts, text.c_str(), &config, nullptr, nullptr);
        if (generated == nullptr) throw std::runtime_error("speech synthesis failed");
        Audio audio;
        audio.samples.assign(generated->samples, generated->samples + generated->n);
        audio.sample_rate = generated->sample_rate;
        SherpaOnnxDestroyOfflineTtsGeneratedAudio(generated);
        return audio;
    }

private:
    LlamaTranslator & llm() {
        std::lock_guard<std::mutex> lock(registry_mutex_);
        if (llm_ == nullptr) {
            const std::string path = (dir_ / llm_model_).string();
            if (!fs::exists(path)) throw ModelError("translation model not found: " + path);
            llm_ = std::make_unique<LlamaTranslator>(path, threads_);
        }
        return *llm_;
    }

    Translator & translator(const std::string & file) {
        std::lock_guard<std::mutex> lock(registry_mutex_);
        auto it = translators_.find(file);
        if (it != translators_.end()) return *it->second;

        auto loaded = std::make_unique<Translator>();
        const std::string path = (dir_ / file).string();
        if (!fs::exists(path)) throw ModelError("translation model not found: " + path);
        if (!sussurro_model_load(path, loaded->model)) throw ModelError("cannot load translation model: " + path);
        loaded->tokenizer = sussurro_tokenizer_create(loaded->model);
        if (loaded->tokenizer == nullptr) throw ModelError("cannot create tokenizer for: " + path);
        return *translators_.emplace(file, std::move(loaded)).first->second;
    }

    Voice & voice(const std::string & lang) {
        std::lock_guard<std::mutex> lock(registry_mutex_);
        auto it = voices_.find(lang);
        if (it != voices_.end()) return *it->second;

        const char * folder = nullptr;
        for (const auto & [code, name] : kVoices) {
            if (lang == code) folder = name;
        }
        if (folder == nullptr) throw Unsupported("no voice for language: " + lang);

        const fs::path voice_dir = dir_ / folder;
        const std::string model = find_onnx(voice_dir);
        if (model.empty()) throw ModelError("no .onnx voice in " + voice_dir.string());
        const std::string tokens = (voice_dir / "tokens.txt").string();
        const std::string data = (voice_dir / "espeak-ng-data").string();

        SherpaOnnxOfflineTtsConfig config;
        std::memset(&config, 0, sizeof(config));
        config.model.vits.model = model.c_str();
        config.model.vits.tokens = tokens.c_str();
        config.model.vits.data_dir = data.c_str();
        config.model.vits.noise_scale = 0.667f;
        config.model.vits.noise_scale_w = 0.8f;
        config.model.vits.length_scale = 1.0f;
        config.model.num_threads = threads_;
        config.model.provider = "cpu";
        config.max_num_sentences = 1;
        config.silence_scale = 0.2f;

        auto loaded = std::make_unique<Voice>();
        loaded->tts = SherpaOnnxCreateOfflineTts(&config);
        if (loaded->tts == nullptr) throw ModelError("cannot load voice: " + model);
        return *voices_.emplace(lang, std::move(loaded)).first->second;
    }

    fs::path dir_;
    std::string whisper_model_;
    std::string llm_model_;
    int threads_;
    whisper_context * whisper_ = nullptr;
    std::mutex whisper_mutex_;
    std::mutex registry_mutex_;
    std::map<std::string, std::unique_ptr<Translator>> translators_;
    std::map<std::string, std::unique_ptr<Voice>> voices_;
    std::unique_ptr<LlamaTranslator> llm_;
};

}

std::unique_ptr<Backend> make_sussurro_backend(const std::string & model_dir, int threads,
                                               const std::string & asr_model, const std::string & translator) {
    return std::make_unique<SussurroBackend>(model_dir, threads, asr_model, translator);
}

}
