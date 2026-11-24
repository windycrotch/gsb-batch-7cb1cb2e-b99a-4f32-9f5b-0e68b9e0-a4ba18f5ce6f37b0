#ifndef AMBIVOX_ENGINE_H
#define AMBIVOX_ENGINE_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

#if defined(AMBIVOX_SHARED) && defined(__GNUC__)
#define AMBIVOX_API __attribute__((visibility("default")))
#else
#define AMBIVOX_API
#endif

#define AMBIVOX_ASR_SAMPLE_RATE 16000

enum ambivox_status {
    AMBIVOX_OK = 0,
    AMBIVOX_ERR_ARGUMENT = 1,
    AMBIVOX_ERR_UNSUPPORTED = 2,
    AMBIVOX_ERR_MODEL = 3,
    AMBIVOX_ERR_INFERENCE = 4
};

typedef struct ambivox_engine ambivox_engine;

typedef struct ambivox_config {
    const char * backend;
    const char * model_dir;
    int threads;
    /* Whisper model name, for ggml-<name>.bin in model_dir; NULL means "small". */
    const char * asr_model;
    /* Translation model: "opus" for the per-pair Marian models, or a name for
       <name>.gguf in model_dir run with llama.cpp. NULL means "opus". */
    const char * translator;
} ambivox_config;

AMBIVOX_API ambivox_engine * ambivox_engine_open(const ambivox_config * config);
AMBIVOX_API void ambivox_engine_close(ambivox_engine * engine);
AMBIVOX_API const char * ambivox_engine_backend(const ambivox_engine * engine);

AMBIVOX_API int ambivox_transcribe(ambivox_engine * engine, const float * pcm, size_t n,
                       const char * lang, char ** text);

AMBIVOX_API int ambivox_translate(ambivox_engine * engine, const char * text,
                      const char * source, const char * target, char ** out);

/* Called with the translation so far each time it grows; the text is valid
   only during the call. */
typedef void (*ambivox_text_fn)(void * user, const char * text);

AMBIVOX_API int ambivox_translate_stream(ambivox_engine * engine, const char * text, const char * source,
                                         const char * target, ambivox_text_fn on_text, void * user,
                                         char ** out);

AMBIVOX_API int ambivox_synthesize(ambivox_engine * engine, const char * text, const char * lang,
                       float ** pcm, size_t * n, int * sample_rate);

AMBIVOX_API void ambivox_free(void * p);

/* Thread-local; valid until the next engine call on the same thread. */
AMBIVOX_API const char * ambivox_last_error(void);

#ifdef __cplusplus
}
#endif

#endif
