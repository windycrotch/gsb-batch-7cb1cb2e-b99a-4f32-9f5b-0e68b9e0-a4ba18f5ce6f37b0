#include "render/pipeline/shadow_pass.h"

#include "base/logger.h"
#include "gfx/command.h"
#include "gfx/debug.h"
#include "gfx/render_target.h"
#include "render/render_data.h"
#include "render/resources/mesh_gpu.h"
#include "render/shaders/embedded_shaders.h"

#include <exception>
#include <glad/glad.h>
#include <glm/gtc/matrix_transform.hpp>

namespace vulcal::render {
namespace {

glm::mat4 light_view_projection(const DirectionalLight& light) {
    const float distance = light.shadow_.far_ * 0.5f;
    const glm::vec3 direction = glm::normalize(light.direction_);
    const glm::vec3 center = light.shadow_.center_;
    const glm::vec3 eye = center - direction * distance;

    const glm::vec3 up = std::abs(glm::dot(direction, glm::vec3{ 0.0f, 1.0f, 0.0f })) > 0.99f
                                 ? glm::vec3{ 0.0f, 0.0f, 1.0f }
                                 : glm::vec3{ 0.0f, 1.0f, 0.0f };

    const glm::mat4 view = glm::lookAt(eye, center, up);
    const float extent = light.shadow_.extent_;
    const glm::mat4 projection = glm::ortho(-extent, extent, -extent, extent, light.shadow_.near_, light.shadow_.far_);
    return projection * view;
}

} // namespace

ShadowPass::ShadowPass()
        : Pass({ .depth_test = true,
              .depth_write = true,
              .cull = false,
              .polygon_offset = true,
              .polygon_offset_factor = 2.0f,
              .polygon_offset_units = 4.0f }) {
    load_shaders(shaders::shadow_vert, shaders::shadow_frag);
}

ShadowPass::~ShadowPass() = default;

bool ShadowPass::enabled(const RenderData& data) const {
    return data.directional_light_ && data.directional_light_->shadow_.enabled_;
}

bool ShadowPass::ensure_target(std::uint32_t map_size) {
    // Common per-frame fast path: the owned map already has the requested size.
    if (target_ && map_size_ == map_size) {
        return true;
    }

    GLint max_texture_size = 0;
    glGetIntegerv(GL_MAX_TEXTURE_SIZE, &max_texture_size);
    const auto max_size = max_texture_size > 0 ? static_cast<std::uint32_t>(max_texture_size) : 0u;

    if (map_size == 0 || (max_size != 0 && map_size > max_size)) {
        base::log::error(
                "Refusing to create shadow map with invalid size {} (allowed range: 1..{}); {}",
                map_size, max_size,
                target_ ? "retaining the previous shadow map" : "no previous shadow map is available");
        return target_ != nullptr;
    }

    gfx::RenderTarget::Spec spec;
    spec.width = map_size;
    spec.height = map_size;
    spec.color_format = std::nullopt;
    spec.depth_format = gfx::Image::Format::Depth32F;

    gfx::Sampler::Spec depth_sampler = gfx::Sampler::Spec::clamp_to_edge();
    depth_sampler.compare_func = gfx::CompareFunc::LessEqual;
    spec.depth_sampler = depth_sampler;

    // Build the replacement before touching the owned target_: if allocation or
    // framebuffer completion throws, the last good map stays bound/owned and
    // the frame can continue using it.
    std::unique_ptr<gfx::RenderTarget> replacement;
    try {
        replacement = std::make_unique<gfx::RenderTarget>(spec);
    } catch (const std::exception& error) {
        base::log::error(
                "Failed to rebuild shadow map at {}x{}: {}; {}", map_size, map_size, error.what(),
                target_ ? "retaining the previous shadow map" : "no previous shadow map is available");
        return target_ != nullptr;
    } catch (...) {
        base::log::error(
                "Failed to rebuild shadow map at {}x{} due to an unknown error; {}", map_size, map_size,
                target_ ? "retaining the previous shadow map" : "no previous shadow map is available");
        return target_ != nullptr;
    }

    gfx::set_object_label(GL_TEXTURE, replacement->depth()->image().id(), "shadow map");
    target_ = std::move(replacement);
    map_size_ = map_size;
    base::log::debug("Shadow map created: {}x{}", map_size, map_size);
    return true;
}

void ShadowPass::prepare(PassContext& ctx) {
    rendering_shadow_ = false;
    // Start every frame without a shadow binding. Only a successfully prepared
    // shadow pass publishes a map to downstream passes, so a disabled/removed
    // light can never leak the previous frame's map through the context.
    ctx.shadow_map_ = nullptr;

    if (!ctx.data_.directional_light_) {
        return;
    }
    const auto& light = *ctx.data_.directional_light_;
    if (!light.shadow_.enabled_) {
        return;
    }

    ctx.light_view_proj_ = light_view_projection(light);

    if (!ensure_target(light.shadow_.map_size_)) {
        // No usable map (e.g. first creation failed). ForwardPass sees a null
        // shadow_map_ and renders unshadowed instead of aborting the frame.
        return;
    }

    ctx.shadow_map_ = target_->depth();

    target_->bind();
    bind_pipeline();
    target_->clear_depth();

    program_.set("u_light_view_proj", ctx.light_view_proj_);
    rendering_shadow_ = true;
}

void ShadowPass::draw(PassContext& ctx, const DrawItem& item) {
    if (!rendering_shadow_) return;
    if (item.kind_ != RenderItemKind::Mesh) return;

    program_.set("u_model", item.world_transform_);
    gfx::Command::draw_indexed(item.mesh_->vertex_array());
}

} // namespace vulcal::render
