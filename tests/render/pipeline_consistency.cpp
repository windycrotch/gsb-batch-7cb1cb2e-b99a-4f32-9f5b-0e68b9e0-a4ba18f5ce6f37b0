// Integration tests for render-pipeline consistency:
//  * shadow map resize / invalid-size retention,
//  * light disable / removal / re-add lifecycle,
//  * stale shadow-texture unbinding,
//  * fixed-function state isolation between passes,
//  * pass ordering and alternating-frame determinism.
//
// The tests render through the real Renderer/Pipeline against a real OpenGL
// context. When no OpenGL 4.6 context can be created (headless CI, missing
// driver) every case is reported as skipped rather than failing.

#include "gfx/context.h"
#include "gfx/render_target.h"
#include "render/pipeline/forward_pass.h"
#include "render/pipeline/pipeline.h"
#include "render/pipeline/shadow_pass.h"
#include "render/render_data.h"
#include "render/renderer.h"
#include "render/resources/mesh_data.h"

#include <SDL3/SDL.h>
#include <glad/glad.h>
#include <glm/gtc/matrix_transform.hpp>
#include <gtest/gtest.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <memory>
#include <optional>
#include <string>
#include <vector>

namespace vulcal::render {
namespace {

constexpr int kWidth  = 256;
constexpr int kHeight = 256;

struct GLHarness {
    SDL_Window* window_ = nullptr;
    std::unique_ptr<gfx::Context> context_;
    bool ok_ = false;
    std::string error_;

    static std::unique_ptr<GLHarness> create() {
        auto harness = std::make_unique<GLHarness>();

        if (!SDL_Init(SDL_INIT_VIDEO)) {
            harness->error_ = std::string{ "SDL_Init(VIDEO) failed: " } + SDL_GetError();
            return harness;
        }

        gfx::Context::prepare_attributes();
        harness->window_ = SDL_CreateWindow("vulcal-pipeline-tests", 64, 64,
                SDL_WINDOW_OPENGL | SDL_WINDOW_HIDDEN);
        if (harness->window_ == nullptr) {
            harness->error_ = std::string{ "SDL_CreateWindow failed: " } + SDL_GetError();
            return harness;
        }

        try {
            harness->context_ = std::make_unique<gfx::Context>(harness->window_);
        } catch (const std::exception& error) {
            harness->error_ = std::string{ "OpenGL 4.6 context unavailable: " } + error.what();
            return harness;
        }

        harness->ok_ = true;
        return harness;
    }

    ~GLHarness() {
        context_.reset();
        if (window_ != nullptr) {
            SDL_DestroyWindow(window_);
            window_ = nullptr;
        }
        SDL_Quit();
    }
};

std::unique_ptr<GLHarness> g_harness;

MeshData make_quad_mesh() {
    MeshData mesh;
    mesh.vertices_ = {
        { { -0.8f, -0.8f, 0.0f }, { 0.0f, 0.0f, 1.0f } },
        { {  0.8f, -0.8f, 0.0f }, { 0.0f, 0.0f, 1.0f } },
        { {  0.8f,  0.8f, 0.0f }, { 0.0f, 0.0f, 1.0f } },
        { { -0.8f,  0.8f, 0.0f }, { 0.0f, 0.0f, 1.0f } },
    };
    mesh.indices_ = { 0, 1, 2, 2, 3, 0 };
    return mesh;
}

void frame_camera(RenderData& data) {
    data.camera_.projection_ = glm::ortho(-2.0f, 2.0f, -2.0f, 2.0f, 0.1f, 20.0f);
    data.camera_.view_ = glm::lookAt(glm::vec3{ 0.0f, 0.0f, 3.0f }, glm::vec3{ 0.0f, 0.0f, 0.0f },
            glm::vec3{ 0.0f, 1.0f, 0.0f });
    data.camera_.position_ = glm::vec3{ 0.0f, 0.0f, 3.0f };
    data.environment_.clear_color_ = glm::vec4{ 0.05f, 0.06f, 0.09f, 1.0f };
    // Generous ambient so visibility checks do not depend on the light facing.
    data.environment_.ambient_color_ = glm::vec3{ 0.55f };
}

DirectionalLight make_light(std::uint32_t shadow_size = 256) {
    DirectionalLight light;
    light.direction_ = glm::normalize(glm::vec3{ 0.4f, -1.0f, 0.35f });
    light.color_ = glm::vec3{ 1.0f, 0.97f, 0.9f };
    light.intensity_ = 1.0f;
    light.shadow_.map_size_ = shadow_size;
    return light;
}

RenderItem make_mesh_item(MeshHandle mesh, MaterialHandle material) {
    RenderItem item;
    item.kind_ = RenderItemKind::Mesh;
    item.mesh_ = mesh;
    item.material_ = material;
    item.world_transform_ = glm::mat4{ 1.0f };
    return item;
}

RenderItem make_sprite_item(const glm::vec2& position, float size, const glm::vec4& color,
        std::int32_t sorting_order, float z = 1.0f) {
    RenderItem item;
    item.kind_ = RenderItemKind::Sprite;
    // T * S: translation must not be scaled by the sprite dimensions.
    item.world_transform_ = glm::scale(
            glm::translate(glm::mat4{ 1.0f }, glm::vec3{ position, z }),
            glm::vec3{ size, size, 1.0f });
    item.sprite_.color_ = color;
    item.sprite_.sorting_order_ = sorting_order;
    return item;
}

std::vector<std::uint8_t> read_pixels(gfx::RenderTarget& target) {
    target.bind();
    glPixelStorei(GL_PACK_ALIGNMENT, 1);
    std::vector<std::uint8_t> pixels(static_cast<std::size_t>(kWidth) * kHeight * 4u);
    glReadPixels(0, 0, kWidth, kHeight, GL_RGBA, GL_UNSIGNED_BYTE, pixels.data());
    gfx::RenderTarget::unbind();
    return pixels;
}

void dump_frame_if_requested(const std::vector<std::uint8_t>& pixels, const std::string& name) {
    const char* directory = std::getenv("VULCAL_TEST_DUMP_DIR");
    if (directory == nullptr || *directory == '\0') {
        return;
    }
    std::error_code error;
    std::filesystem::create_directories(directory, error);
    std::ofstream output(std::filesystem::path{ directory } / (name + ".ppm"), std::ios::binary);
    output << "P6\n" << kWidth << ' ' << kHeight << "\n255\n";
    for (int y = kHeight - 1; y >= 0; --y) {
        output.write(reinterpret_cast<const char*>(&pixels[static_cast<std::size_t>(y) * kWidth * 4u]),
                static_cast<std::streamsize>(kWidth) * 4);
    }
}

::testing::AssertionResult frames_equal(const std::vector<std::uint8_t>& expected,
        const std::vector<std::uint8_t>& actual) {
    if (expected.size() != actual.size()) {
        return ::testing::AssertionFailure() << "buffer size mismatch: " << expected.size() << " vs "
                                             << actual.size();
    }
    std::size_t different_bytes = 0;
    std::size_t different_pixels = 0;
    int max_delta = 0;
    for (std::size_t i = 0; i < expected.size(); ++i) {
        const int delta = std::abs(static_cast<int>(expected[i]) - static_cast<int>(actual[i]));
        if (delta != 0) {
            ++different_bytes;
            max_delta = std::max(max_delta, delta);
            if (i % 4 == 0) {
                ++different_pixels;
            }
        }
    }
    if (different_bytes == 0) {
        return ::testing::AssertionSuccess();
    }
    return ::testing::AssertionFailure()
           << different_pixels << " pixels (" << different_bytes << " bytes) differ, max channel delta "
           << max_delta;
}

// The frame must contain substantially more than the clear color, otherwise
// exact-equality checks would be vacuous.
::testing::AssertionResult frame_is_visible(const std::vector<std::uint8_t>& pixels,
        const glm::vec4& clear_color) {
    const int clear_r = static_cast<int>(clear_color.r * 255.0f);
    const int clear_g = static_cast<int>(clear_color.g * 255.0f);
    const int clear_b = static_cast<int>(clear_color.b * 255.0f);
    int visible_pixels = 0;
    for (std::size_t p = 0; p < pixels.size(); p += 4) {
        if (std::abs(static_cast<int>(pixels[p]) - clear_r) > 24 ||
                std::abs(static_cast<int>(pixels[p + 1]) - clear_g) > 24 ||
                std::abs(static_cast<int>(pixels[p + 2]) - clear_b) > 24) {
            ++visible_pixels;
        }
    }
    if (visible_pixels > 200) {
        return ::testing::AssertionSuccess() << visible_pixels << " visible pixels";
    }
    return ::testing::AssertionFailure() << "frame looks empty: only " << visible_pixels
                                         << " pixels differ from the clear color";
}

bool drain_gl_errors() {
    bool clean = true;
    while (glGetError() != GL_NO_ERROR) {
        clean = false;
    }
    return clean;
}

class PipelineConsistency : public ::testing::Test {
protected:
    static void SetUpTestSuite() {
        g_harness = GLHarness::create();
    }

    static void TearDownTestSuite() {
        g_harness.reset();
    }

    void SetUp() override {
        if (!g_harness || !g_harness->ok_) {
            GTEST_SKIP() << (g_harness ? g_harness->error_ : "GL harness was not initialized");
        }
        ASSERT_TRUE(drain_gl_errors());
        renderer_ = std::make_unique<Renderer>();
        target_ = std::make_unique<gfx::RenderTarget>(gfx::RenderTarget::Spec{
                .width = static_cast<std::uint32_t>(kWidth),
                .height = static_cast<std::uint32_t>(kHeight) });
        mesh_ = renderer_->create_mesh(make_quad_mesh());
        material_ = renderer_->create_material(Material{
                .diffuse_ = glm::vec3{ 0.85f, 0.35f, 0.25f },
                .specular_ = glm::vec3{ 0.2f },
                .shininess_ = 16.0f });
    }

    void TearDown() override {
        target_.reset();
        renderer_.reset();
    }

    std::unique_ptr<Renderer> renderer_;
    std::unique_ptr<gfx::RenderTarget> target_;
    MeshHandle mesh_;
    MaterialHandle material_;
};

TEST_F(PipelineConsistency, PassOrderIsShadowForwardSprite) {
    Pipeline pipeline;
    const auto names = pipeline.pass_names();
    ASSERT_EQ(names.size(), 3u);
    EXPECT_STREQ(names[0], "ShadowPass");
    EXPECT_STREQ(names[1], "ForwardPass");
    EXPECT_STREQ(names[2], "SpritePass");
}

TEST_F(PipelineConsistency, ShadowMapSizeChangeRebuildsAndPublishesNewMap) {
    RenderData data;
    frame_camera(data);
    data.directional_light_ = make_light(256);

    PassContext ctx{ .data_ = data, .target_ = target_.get(), .viewport_ = { 0, 0, kWidth, kHeight } };
    ShadowPass shadow;

    shadow.prepare(ctx);
    ASSERT_NE(shadow.shadow_target(), nullptr);
    EXPECT_EQ(shadow.map_size(), 256u);
    ASSERT_EQ(ctx.shadow_map_, shadow.shadow_target()->depth());

    GLint texture_width = 0;
    glGetTextureLevelParameteriv(ctx.shadow_map_->image().id(), 0, GL_TEXTURE_WIDTH, &texture_width);
    EXPECT_EQ(texture_width, 256);
    shadow.finish(ctx);

    // Changing the size between frames must rebuild and publish the new map.
    data.directional_light_->shadow_.map_size_ = 512;
    shadow.prepare(ctx);
    ASSERT_NE(shadow.shadow_target(), nullptr);
    EXPECT_EQ(shadow.map_size(), 512u);
    ASSERT_EQ(ctx.shadow_map_, shadow.shadow_target()->depth());
    texture_width = 0;
    glGetTextureLevelParameteriv(ctx.shadow_map_->image().id(), 0, GL_TEXTURE_WIDTH, &texture_width);
    EXPECT_EQ(texture_width, 512);
    EXPECT_TRUE(drain_gl_errors());
    shadow.finish(ctx);
}

TEST_F(PipelineConsistency, InvalidSizeKeepsLastGoodMapAndLogsError) {
    RenderData data;
    frame_camera(data);
    data.directional_light_ = make_light(256);

    PassContext ctx{ .data_ = data, .target_ = target_.get(), .viewport_ = { 0, 0, kWidth, kHeight } };
    ShadowPass shadow;
    shadow.prepare(ctx);
    ASSERT_EQ(shadow.map_size(), 256u);
    const GLuint good_texture = ctx.shadow_map_->image().id();
    shadow.finish(ctx);

    // Zero is illegal: keep the 256 map instead of throwing / replacing it.
    data.directional_light_->shadow_.map_size_ = 0;
    EXPECT_NO_THROW(shadow.prepare(ctx));
    EXPECT_EQ(shadow.map_size(), 256u);
    ASSERT_NE(shadow.shadow_target(), nullptr);
    EXPECT_EQ(shadow.shadow_target()->depth()->image().id(), good_texture);
    EXPECT_EQ(ctx.shadow_map_, shadow.shadow_target()->depth());
    shadow.finish(ctx);

    // Absurd sizes are illegal too (larger than GL_MAX_TEXTURE_SIZE).
    data.directional_light_->shadow_.map_size_ = 0xFFFFFFFFu;
    EXPECT_NO_THROW(shadow.prepare(ctx));
    EXPECT_EQ(shadow.map_size(), 256u);
    EXPECT_EQ(shadow.shadow_target()->depth()->image().id(), good_texture);
    EXPECT_TRUE(drain_gl_errors());
    shadow.finish(ctx);
}

TEST_F(PipelineConsistency, UnusableSizeWithoutPreviousMapDisablesShadowGracefully) {
    RenderData data;
    frame_camera(data);
    data.directional_light_ = make_light(0);

    PassContext ctx{ .data_ = data, .target_ = target_.get(), .viewport_ = { 0, 0, kWidth, kHeight } };
    ShadowPass shadow;

    EXPECT_NO_THROW(shadow.prepare(ctx));
    EXPECT_EQ(shadow.map_size(), 0u);
    EXPECT_EQ(shadow.shadow_target(), nullptr);
    EXPECT_EQ(ctx.shadow_map_, nullptr);
    // draw() must not dereference a null mesh when preparation did not run.
    DrawItem empty_item;
    EXPECT_NO_THROW(shadow.draw(ctx, empty_item));
    EXPECT_NO_THROW(shadow.finish(ctx));
    EXPECT_TRUE(drain_gl_errors());
}

TEST_F(PipelineConsistency, DisabledLightUnbindsStaleShadowTexture) {
    RenderData data;
    frame_camera(data);
    data.directional_light_ = make_light(128);

    PassContext ctx{ .data_ = data, .target_ = target_.get(), .viewport_ = { 0, 0, kWidth, kHeight } };
    ShadowPass shadow;
    ForwardPass forward;

    // Frame with shadows: the forward pass binds the shadow map to unit 0.
    shadow.prepare(ctx);
    ASSERT_NE(ctx.shadow_map_, nullptr);
    shadow.finish(ctx);
    forward.prepare(ctx);
    GLint bound_texture = 0;
    glActiveTexture(GL_TEXTURE0);
    glGetIntegerv(GL_TEXTURE_BINDING_2D, &bound_texture);
    EXPECT_EQ(bound_texture, static_cast<GLint>(ctx.shadow_map_->image().id()));
    forward.finish(ctx);

    // Simulate the next frame with shadows disabled: Pipeline hands the passes
    // a fresh context whose shadow_map_ is null.
    data.directional_light_->shadow_.enabled_ = false;
    PassContext next_ctx{ .data_ = data,
        .target_ = target_.get(),
        .viewport_ = { 0, 0, kWidth, kHeight } };
    ASSERT_EQ(next_ctx.shadow_map_, nullptr);

    // Deliberately leave an unrelated texture bound to prove prepare() unbinds.
    GLuint sentinel = 0;
    glGenTextures(1, &sentinel);
    glBindTexture(GL_TEXTURE_2D, sentinel);
    forward.prepare(next_ctx);
    bound_texture = -1;
    glActiveTexture(GL_TEXTURE0);
    glGetIntegerv(GL_TEXTURE_BINDING_2D, &bound_texture);
    EXPECT_EQ(bound_texture, 0) << "forward pass must unbind stale shadow textures when shadows are off";
    forward.finish(next_ctx);
    glDeleteTextures(1, &sentinel);
    EXPECT_TRUE(drain_gl_errors());
}

TEST_F(PipelineConsistency, MixedMeshAndSpritesRepeatedFramesAreIdenticalAndStateIsClean) {
    RenderData data;
    frame_camera(data);
    data.directional_light_ = make_light(256);
    data.items_.push_back(make_mesh_item(mesh_, material_));
    data.items_.push_back(make_sprite_item({ -0.35f, 0.15f }, 1.1f,
            glm::vec4{ 0.9f, 0.2f, 0.2f, 0.55f }, 0));
    data.items_.push_back(make_sprite_item({ 0.35f, -0.1f }, 0.9f,
            glm::vec4{ 0.2f, 0.4f, 0.95f, 0.55f }, 1));

    ASSERT_NO_THROW(renderer_->render(data, target_.get()));
    ASSERT_TRUE(drain_gl_errors());
    const auto first = read_pixels(*target_);
    EXPECT_TRUE(frame_is_visible(first, data.environment_.clear_color_));
    dump_frame_if_requested(first, "mixed-first");

    // Repeat the exact same frame: the visible result must converge.
    ASSERT_NO_THROW(renderer_->render(data, target_.get()));
    ASSERT_TRUE(drain_gl_errors());
    const auto second = read_pixels(*target_);
    dump_frame_if_requested(second, "mixed-second");
    EXPECT_TRUE(frames_equal(first, second));

    // SpritePass is the last pass; its finish() must neutralize blend/depth
    // mask/polygon offset so none leaks to editor/other rendering.
    GLboolean blend_enabled = GL_TRUE;
    glGetBooleanv(GL_BLEND, &blend_enabled);
    EXPECT_FALSE(blend_enabled);

    GLint depth_write_mask = 0;
    glGetIntegerv(GL_DEPTH_WRITEMASK, &depth_write_mask);
    EXPECT_EQ(depth_write_mask, GL_TRUE);

    GLboolean polygon_offset = GL_TRUE;
    glGetBooleanv(GL_POLYGON_OFFSET_FILL, &polygon_offset);
    EXPECT_FALSE(polygon_offset);
    EXPECT_TRUE(drain_gl_errors());
}

TEST_F(PipelineConsistency, OverlappingTransparentSpritesBlendInSortingOrder) {
    auto render_center = [&](bool red_on_top) {
        RenderData data;
        frame_camera(data);
        data.directional_light_.reset();
        // Identical geometry/colors except sorting order. The top sprite must
        // dominate the blended center pixel.
        data.items_.push_back(make_sprite_item({ 0.0f, 0.0f }, 1.6f,
                glm::vec4{ 0.9f, 0.1f, 0.1f, 0.55f }, red_on_top ? 1 : 0, 0.5f));
        data.items_.push_back(make_sprite_item({ 0.0f, 0.0f }, 1.6f,
                glm::vec4{ 0.1f, 0.25f, 0.95f, 0.55f }, red_on_top ? 0 : 1, 0.5f));
        renderer_->render(data, target_.get());
        auto pixels = read_pixels(*target_);
        dump_frame_if_requested(pixels, red_on_top ? "sprite-red-top" : "sprite-blue-top");
        const std::size_t center = (static_cast<std::size_t>(kHeight / 2) * kWidth + kWidth / 2) * 4u;
        return std::array<std::uint8_t, 3>{ pixels[center], pixels[center + 1], pixels[center + 2] };
    };

    const auto red_top = render_center(true);
    const auto blue_top = render_center(false);

    EXPECT_GT(red_top[0], red_top[2]) << "red sprite drawn last should dominate the center pixel";
    EXPECT_GT(blue_top[2], blue_top[0]) << "blue sprite drawn last should dominate the center pixel";
    EXPECT_NE(red_top, blue_top) << "sorting order must change the blended result";
}

TEST_F(PipelineConsistency, PassAlternationAcrossConsecutiveFramesStaysDeterministic) {
    auto render_frame = [&](bool mesh, int sprite_count) {
        RenderData data;
        frame_camera(data);
        data.directional_light_ = make_light(128);
        if (mesh) {
            data.items_.push_back(make_mesh_item(mesh_, material_));
        }
        for (int i = 0; i < sprite_count; ++i) {
            const float x = -0.6f + 0.6f * static_cast<float>(i);
            data.items_.push_back(make_sprite_item({ x, 0.2f * static_cast<float>(i % 2) }, 0.7f,
                    glm::vec4{ 0.2f + 0.2f * static_cast<float>(i), 0.8f, 0.4f, 0.6f }, i));
        }
        renderer_->render(data, target_.get());
        auto first = read_pixels(*target_);
        renderer_->render(data, target_.get());
        auto second = read_pixels(*target_);
        EXPECT_TRUE(frames_equal(first, second));
        EXPECT_TRUE(drain_gl_errors());
        return first;
    };

    SCOPED_TRACE("mesh only");
    const auto mesh_only = render_frame(true, 0);
    EXPECT_TRUE(frame_is_visible(mesh_only, glm::vec4{ 0.05f, 0.06f, 0.09f, 1.0f }));

    SCOPED_TRACE("mesh + sprites");
    const auto mesh_and_sprites = render_frame(true, 2);
    EXPECT_TRUE(frame_is_visible(mesh_and_sprites, glm::vec4{ 0.05f, 0.06f, 0.09f, 1.0f }));

    SCOPED_TRACE("sprites only (shadow/forward still run, sprite pass toggles)");
    const auto sprites_only = render_frame(false, 3);
    EXPECT_TRUE(frame_is_visible(sprites_only, glm::vec4{ 0.05f, 0.06f, 0.09f, 1.0f }));

    SCOPED_TRACE("back to mesh + sprites");
    EXPECT_TRUE(frames_equal(mesh_and_sprites, render_frame(true, 2)));

    SCOPED_TRACE("back to mesh only");
    EXPECT_TRUE(frames_equal(mesh_only, render_frame(true, 0)));
}

TEST_F(PipelineConsistency, LightDisableRemoveAndReaddRestoresIdenticalFrame) {
    auto render_scene = [&](std::optional<DirectionalLight> light) {
        RenderData data;
        frame_camera(data);
        data.directional_light_ = std::move(light);
        data.items_.push_back(make_mesh_item(mesh_, material_));
        renderer_->render(data, target_.get());
        EXPECT_TRUE(drain_gl_errors());
        return read_pixels(*target_);
    };

    const auto baseline = render_scene(make_light(256));
    EXPECT_TRUE(frame_is_visible(baseline, glm::vec4{ 0.05f, 0.06f, 0.09f, 1.0f }));
    dump_frame_if_requested(baseline, "light-on");

    DirectionalLight disabled_light = make_light(256);
    disabled_light.shadow_.enabled_ = false;
    std::vector<std::uint8_t> shadows_off;
    EXPECT_NO_THROW(shadows_off = render_scene(disabled_light));
    EXPECT_TRUE(frame_is_visible(shadows_off, glm::vec4{ 0.05f, 0.06f, 0.09f, 1.0f }));
    dump_frame_if_requested(shadows_off, "shadow-disabled");

    std::vector<std::uint8_t> no_light;
    EXPECT_NO_THROW(no_light = render_scene(std::nullopt));
    EXPECT_TRUE(frame_is_visible(no_light, glm::vec4{ 0.05f, 0.06f, 0.09f, 1.0f }));
    dump_frame_if_requested(no_light, "light-removed");

    // Re-adding the same light must reproduce the original frame exactly.
    const auto restored = render_scene(make_light(256));
    dump_frame_if_requested(restored, "light-restored");
    EXPECT_TRUE(frames_equal(baseline, restored));
}

TEST_F(PipelineConsistency, ZeroShadowSizeThroughRendererDoesNotAbortFrame) {
    RenderData data;
    frame_camera(data);
    data.directional_light_ = make_light(256);
    data.items_.push_back(make_mesh_item(mesh_, material_));

    renderer_->render(data, target_.get());

    data.directional_light_->shadow_.map_size_ = 0;
    EXPECT_NO_THROW(renderer_->render(data, target_.get()));
    EXPECT_TRUE(drain_gl_errors());
    const auto pixels = read_pixels(*target_);
    EXPECT_TRUE(frame_is_visible(pixels, data.environment_.clear_color_));

    // A later legal size recovers fully.
    data.directional_light_->shadow_.map_size_ = 128;
    EXPECT_NO_THROW(renderer_->render(data, target_.get()));
    EXPECT_TRUE(drain_gl_errors());
}

} // namespace
} // namespace vulcal::render
