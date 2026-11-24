#include "ambivox/engine.h"

#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <exception>
#include <string>

#include "backend.h"

struct ambivox_engine {
    std::unique_ptr<ambivox::Backend> backend;
};

namespace {

thread_local std::string last_error;

int fail(int status, const std::string & message) {
    last_error = message;
    return status;
}

template <typename F>
int guarded(F && body) {
    last_error.clear();
    try {
        body();
        return AMBIVOX_OK;
    } catch (const ambivox::Unsupported & e) {
        return fail(AMBIVOX_ERR_UNSUPPORTED, e.what());
    } catch (const ambivox::ModelError & e) {
        return fail(AMBIVOX_ERR_MODEL, e.what());
    } catch (const std::exception & e) {
        return fail(AMBIVOX_ERR_INFERENCE, e.what());
    } catch (...) {
        return fail(AMBIVOX_ERR_INFERENCE, "unknown failure");
    }
}

char * copy_string(const std::string & s) {
    char * out = (char *) std::malloc(s.size() + 1);
    if (out == nullptr) throw std::bad_alloc();
    std::memcpy(out, s.c_str(), s.size() + 1);
    return out;
}

}

extern "C" {

ambivox_engine * ambivox_engine_open(const ambivox_config * config) {
    ambivox_engine * engine = nullptr;
    const int status = guarded([&] {
        const std::string backend = config != nullptr && config->backend != nullptr ? config->backend : "sim";
        std::unique_ptr<ambivox::Backend> impl;
        if (backend == "sim") {
            impl = ambivox::make_sim_backend();
        } else if (backend == "sussurro") {
#ifdef AMBIVOX_WITH_SUSSURRO
            if (config->model_dir == nullptr) throw ambivox::ModelError("the sussurro backend needs a model directory");
            impl = ambivox::make_sussurro_backend(config->model_dir, config->threads,
                                                  config->asr_model != nullptr ? config->asr_model : "small",
                                                  config->translator != nullptr ? config->translator : "opus");
#else
            throw ambivox::Unsupported("built without the sussurro backend");
#endif
        } else {
            throw ambivox::Unsupported("unknown backend: " + backend);
        }
        engine = new ambivox_engine{std::move(impl)};
    });
    return status == AMBIVOX_OK ? engine : nullptr;
}

void ambivox_engine_close(ambivox_engine * engine) { delete engine; }

const char * ambivox_engine_backend(const ambivox_engine * engine) {
    return engine != nullptr ? engine->backend->name() : "";
}

int ambivox_transcribe(ambivox_engine * engine, const float * pcm, size_t n, const char * lang, char ** text) {
    if (engine == nullptr || (pcm == nullptr && n > 0) || lang == nullptr || text == nullptr) {
        return fail(AMBIVOX_ERR_ARGUMENT, "invalid argument");
    }
    *text = nullptr;
    return guarded([&] { *text = copy_string(engine->backend->transcribe(pcm, n, lang)); });
}

int ambivox_translate(ambivox_engine * engine, const char * text, const char * source, const char * target,
                      char ** out) {
    if (engine == nullptr || text == nullptr || source == nullptr || target == nullptr || out == nullptr) {
        return fail(AMBIVOX_ERR_ARGUMENT, "invalid argument");
    }
    *out = nullptr;
    return guarded([&] { *out = copy_string(engine->backend->translate(text, source, target)); });
}

int ambivox_translate_stream(ambivox_engine * engine, const char * text, const char * source, const char * target,
                             ambivox_text_fn on_text, void * user, char ** out) {
    if (engine == nullptr || text == nullptr || source == nullptr || target == nullptr || out == nullptr) {
        return fail(AMBIVOX_ERR_ARGUMENT, "invalid argument");
    }
    *out = nullptr;
    return guarded([&] {
        ambivox::TextFn report;
        if (on_text != nullptr) report = [&](const std::string & so_far) { on_text(user, so_far.c_str()); };
        *out = copy_string(engine->backend->translate_stream(text, source, target, report));
    });
}

int ambivox_synthesize(ambivox_engine * engine, const char * text, const char * lang, float ** pcm, size_t * n,
                       int * sample_rate) {
    if (engine == nullptr || text == nullptr || lang == nullptr || pcm == nullptr || n == nullptr ||
        sample_rate == nullptr) {
        return fail(AMBIVOX_ERR_ARGUMENT, "invalid argument");
    }
    *pcm = nullptr;
    *n = 0;
    *sample_rate = 0;
    return guarded([&] {
        const ambivox::Audio audio = engine->backend->synthesize(text, lang);
        float * samples = (float *) std::malloc(std::max<size_t>(audio.samples.size(), 1) * sizeof(float));
        if (samples == nullptr) throw std::bad_alloc();
        std::memcpy(samples, audio.samples.data(), audio.samples.size() * sizeof(float));
        *pcm = samples;
        *n = audio.samples.size();
        *sample_rate = audio.sample_rate;
    });
}

void ambivox_free(void * p) { std::free(p); }

const char * ambivox_last_error(void) { return last_error.c_str(); }

}
