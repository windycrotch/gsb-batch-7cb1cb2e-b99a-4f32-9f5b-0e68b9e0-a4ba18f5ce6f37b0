use std::env;

fn main() {
    if env::var_os("CARGO_FEATURE_SUSSURRO").is_some()
        && let Some(dir) = env::var_os("DEP_AMBIVOX_ENGINE_LIBDIR")
    {
        println!("cargo:rustc-link-arg=-Wl,-rpath,{}", dir.to_string_lossy());
    }
}
