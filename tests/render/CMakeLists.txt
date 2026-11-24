add_executable(embedded_shaders_test embedded_shaders.cpp)
target_include_directories(embedded_shaders_test PRIVATE "${PROJECT_BINARY_DIR}/vulcal_engine/generated")
target_link_libraries(embedded_shaders_test PRIVATE GTest::gtest_main)
file(TO_CMAKE_PATH "${PROJECT_SOURCE_DIR}/vulcal_engine/render/shaders" VULCAL_TEST_SHADER_DIR)
target_compile_definitions(embedded_shaders_test PRIVATE VULCAL_TEST_SHADER_DIR="${VULCAL_TEST_SHADER_DIR}")

# Real-GL pipeline consistency tests. Cases skip themselves when no OpenGL 4.6
# context can be created (e.g. headless CI), so the target is always built.
add_executable(pipeline_consistency_test pipeline_consistency.cpp)
target_link_libraries(pipeline_consistency_test PRIVATE vulcal_engine GTest::gtest_main)

include(GoogleTest)
gtest_discover_tests(embedded_shaders_test)
gtest_discover_tests(pipeline_consistency_test PROPERTIES LABELS graphics RUN_SERIAL TRUE)

if(MINGW)
    # Static runtime linkage keeps the test executables independent of other
    # MinGW installations that may appear earlier on PATH (e.g. Git's runtime).
    target_link_options(embedded_shaders_test PRIVATE -static-libgcc -static-libstdc++)
    target_link_options(pipeline_consistency_test PRIVATE -static-libgcc -static-libstdc++)
endif()
