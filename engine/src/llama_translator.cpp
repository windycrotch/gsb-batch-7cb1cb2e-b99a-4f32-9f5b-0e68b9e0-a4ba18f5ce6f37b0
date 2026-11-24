#include "llama_translator.h"

#include <algorithm>
#include <cstdio>
#include <mutex>
#include <vector>

#include "backend.h"
#include "llama.h"

namespace ambivox {

namespace {

constexpr int kContext = 2048;
constexpr int kMaxNewTokens = 256;

constexpr std::pair<const char *, const char *> kNames[] = {
    {"en", "English"},
    {"fr", "French"},
    {"es", "Spanish"},
    {"it", "Italian"},
};

std::string language_name(const std::string & code) {
    for (const auto & [known, name] : kNames) {
        if (code == known) return name;
    }
    throw Unsupported("no language name for: " + code);
}

void quiet(ggml_log_level level, const char * text, void *) {
    if (level == GGML_LOG_LEVEL_ERROR) fputs(text, stderr);
}

size_t complete_utf8(const std::string & text) {
    for (size_t back = 1; back <= 4 && back <= text.size(); ++back) {
        const unsigned char c = text[text.size() - back];
        if ((c & 0xC0) == 0x80) continue;
        const size_t length = c >= 0xF0 ? 4 : c >= 0xE0 ? 3 : c >= 0xC0 ? 2 : 1;
        return length > back ? text.size() - back : text.size();
    }
    return text.size();
}

class Model {
public:
    Model(const std::string & path, int threads) {
        llama_model_params model_params = llama_model_default_params();
        model_params.n_gpu_layers = -1;
        model_ = llama_model_load_from_file(path.c_str(), model_params);
        if (model_ == nullptr) throw ModelError("cannot load translation model: " + path);
        vocab_ = llama_model_get_vocab(model_);

        llama_context_params context_params = llama_context_default_params();
        context_params.n_ctx = kContext;
        context_params.n_batch = kContext;
        context_params.n_threads = threads;
        context_params.n_threads_batch = threads;
        context_params.swa_full = true;
        context_params.no_perf = true;
        context_ = llama_init_from_model(model_, context_params);
        if (context_ == nullptr) throw ModelError("cannot create a context for: " + path);
        batch_ = llama_batch_init(kContext, 0, 1);
    }

    ~Model() {
        llama_batch_free(batch_);
        if (context_ != nullptr) llama_free(context_);
        if (model_ != nullptr) llama_model_free(model_);
    }

    Model(const Model &) = delete;
    Model & operator=(const Model &) = delete;

    const llama_vocab * vocab() const { return vocab_; }

    void feed(const std::vector<llama_token> & tokens) {
        const size_t limit = tokens.size() - 1;
        size_t kept = 0;
        while (kept < limit && kept < cached_.size() && cached_[kept] == tokens[kept]) ++kept;
        llama_memory_t memory = llama_get_memory(context_);
        if (!llama_memory_seq_rm(memory, 0, (llama_pos) kept, -1)) {
            llama_memory_clear(memory, true);
            kept = 0;
        }
        batch_.n_tokens = 0;
        for (size_t i = kept; i < tokens.size(); ++i) {
            const int at = batch_.n_tokens++;
            batch_.token[at] = tokens[i];
            batch_.pos[at] = (llama_pos) i;
            batch_.n_seq_id[at] = 1;
            batch_.seq_id[at][0] = 0;
            batch_.logits[at] = i == limit;
        }
        cached_ = tokens;
        if (llama_decode(context_, batch_) != 0) {
            cached_.clear();
            llama_memory_clear(memory, true);
            throw std::runtime_error("translation failed");
        }
    }

    llama_token best() const {
        const float * logits = llama_get_logits_ith(context_, -1);
        return (llama_token) (std::max_element(logits, logits + llama_vocab_n_tokens(vocab_)) - logits);
    }

private:
    llama_model * model_ = nullptr;
    llama_context * context_ = nullptr;
    const llama_vocab * vocab_ = nullptr;
    llama_batch batch_{};
    std::vector<llama_token> cached_;
};

}

std::string translategemma_prompt(const std::string & text, const std::string & source, const std::string & target) {
    const std::string from = language_name(source);
    const std::string to = language_name(target);
    return "<start_of_turn>user\nYou are a professional " + from + " (" + source + ") to " + to + " (" + target +
           ") translator. Your goal is to accurately convey the meaning and nuances of the original " + from +
           " text while adhering to " + to + " grammar, vocabulary, and cultural sensitivities.\nProduce only the " +
           to + " translation, without any additional explanations or commentary. Please translate the following " +
           from + " text into " + to + ":\n\n\n" + text + "<end_of_turn>\n<start_of_turn>model\n";
}

struct LlamaTranslator::State {
    std::unique_ptr<Model> model;
    std::mutex mutex;
};

LlamaTranslator::LlamaTranslator(const std::string & path, int threads) : state_(std::make_unique<State>()) {
    static std::once_flag initialized;
    std::call_once(initialized, [] {
        llama_log_set(quiet, nullptr);
        llama_backend_init();
    });

    state_->model = std::make_unique<Model>(path, threads);
}

LlamaTranslator::~LlamaTranslator() = default;

std::string LlamaTranslator::translate(const std::string & text, const std::string & source,
                                       const std::string & target,
                                       const std::function<void(const std::string &)> & on_text) {
    const std::string prompt = translategemma_prompt(text, source, target);
    std::lock_guard<std::mutex> lock(state_->mutex);
    Model & model = *state_->model;
    const llama_vocab * vocab = model.vocab();

    const int needed = -llama_tokenize(vocab, prompt.c_str(), (int32_t) prompt.size(), nullptr, 0, true, true);
    if (needed <= 0 || needed + kMaxNewTokens > kContext) throw Unsupported("text too long to translate");
    std::vector<llama_token> tokens(needed);
    llama_tokenize(vocab, prompt.c_str(), (int32_t) prompt.size(), tokens.data(), needed, true, true);
    const size_t prompt_size = tokens.size();

    std::string out;
    char piece[256];
    while (tokens.size() - prompt_size < kMaxNewTokens) {
        model.feed(tokens);
        const llama_token token = model.best();
        if (llama_vocab_is_eog(vocab, token)) break;
        tokens.push_back(token);
        const int n = llama_token_to_piece(vocab, token, piece, sizeof(piece), 0, false);
        if (n > 0) out.append(piece, n);
        if (on_text) on_text(out.substr(0, complete_utf8(out)));
    }
    return out;
}

}
