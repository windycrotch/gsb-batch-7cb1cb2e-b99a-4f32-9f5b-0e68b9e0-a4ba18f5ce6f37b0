#pragma once

#include <functional>
#include <memory>
#include <string>

namespace ambivox {

class LlamaTranslator {
public:
    LlamaTranslator(const std::string & path, int threads);
    ~LlamaTranslator();
    LlamaTranslator(const LlamaTranslator &) = delete;
    LlamaTranslator & operator=(const LlamaTranslator &) = delete;

    std::string translate(const std::string & text, const std::string & source, const std::string & target,
                          const std::function<void(const std::string &)> & on_text = {});

private:
    struct State;
    std::unique_ptr<State> state_;
};

std::string translategemma_prompt(const std::string & text, const std::string & source, const std::string & target);

}
