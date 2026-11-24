#include "render/pipeline/pass.h"

#include "base/logger.h"
#include "gfx/command.h"
#include "gfx/shader.h"

namespace vulcal::render {

Pass::Pass(const gfx::PipelineState::Spec& pipeline_spec) : pipeline_state_(pipeline_spec) {}

void Pass::load_shaders(const char* vertex_source, const char* fragment_source) {
    base::log::debug("Compiling embedded shaders for pass '{}'", name());
    gfx::Shader vertex_shader(gfx::Shader::Type::Vertex, vertex_source);
    gfx::Shader fragment_shader(gfx::Shader::Type::Fragment, fragment_source);
    program_.attach(vertex_shader);
    program_.attach(fragment_shader);
    program_.link();
}

void Pass::bind_pipeline() {
    gfx::PipelineState::reset_cache();
    gfx::Command::apply(pipeline_state_);
    program_.use();
}

void Pass::finish(PassContext& /*ctx*/) {
    reset_gpu_state();
}

void Pass::reset_gpu_state() {
    // Re-issue every fixed-function toggle from a neutral spec. Invalidating
    // the global state cache first guarantees the driver actually receives the
    // calls even if another pass left the cache out of sync with the GL state.
    static const gfx::PipelineState neutral{ gfx::PipelineState::Spec{} };
    gfx::PipelineState::reset_cache();
    gfx::Command::apply(neutral);
    gfx::Program::unuse();
    gfx::VertexArray::unbind();
}

} // namespace vulcal::render
