#pragma once
#include "render/pipeline/pass.h"
#include <cstdint>
#include <memory>

namespace vulcal::gfx {
class RenderTarget;
} // namespace vulcal::gfx

namespace vulcal::render {

class ShadowPass : public Pass {
public:
    ShadowPass();
    ~ShadowPass() override;

    const char* name() const override { return "ShadowPass"; }
    bool enabled(const RenderData& data) const override;

    void prepare(PassContext& ctx) override;
    void draw(PassContext& ctx, const DrawItem& item) override;

    // Requested size of the currently owned, usable shadow map (0 before the
    // first successful creation). An invalid resize keeps the previous size.
    std::uint32_t map_size() const { return map_size_; }
    const gfx::RenderTarget* shadow_target() const { return target_.get(); }
    // True only between a successful prepare() and the next prepare()/finish().
    bool is_rendering_shadow() const { return rendering_shadow_; }

private:
    // Returns true when a usable target is owned after the call. Illegal sizes
    // or target creation failures are logged and leave any previous target
    // intact instead of propagating an exception.
    bool ensure_target(std::uint32_t map_size);

    std::unique_ptr<gfx::RenderTarget> target_;
    std::uint32_t map_size_ = 0;
    bool rendering_shadow_ = false;
};

} // namespace vulcal::render
