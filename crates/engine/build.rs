use std::env;
use std::path::PathBuf;

fn main() {
    let source = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap()).join("../../engine");
    let source = source.canonicalize().expect("engine sources not found");
    let sussurro = env::var_os("CARGO_FEATURE_SUSSURRO").is_some();

    for dir in ["CMakeLists.txt", "include", "src"] {
        println!("cargo:rerun-if-changed={}", source.join(dir).display());
    }
    println!("cargo:rerun-if-env-changed=AMBIVOX_SHERPA_DIR");
    println!("cargo:rerun-if-env-changed=AMBIVOX_PORTABLE");

    let mut config = cmake::Config::new(&source);
    config.profile("Release").define("AMBIVOX_BUILD_TESTS", "OFF");
    if sussurro {
        config.define("AMBIVOX_BACKEND_SUSSURRO", "ON");
        if let Some(dir) = env::var_os("AMBIVOX_SHERPA_DIR") {
            config.define("AMBIVOX_SHERPA_DIR", dir);
        }
        if env::var_os("AMBIVOX_PORTABLE").is_some_and(|v| v == "1") {
            config.define("AMBIVOX_PORTABLE", "ON");
        }
    }
    let lib = config.build().join("lib");

    println!("cargo:rustc-link-search=native={}", lib.display());
    println!("cargo:libdir={}", lib.display());

    if sussurro {
        println!("cargo:rustc-link-lib=dylib=ambivox_engine");
        println!("cargo:rustc-link-arg=-Wl,-rpath,{}", lib.display());
        return;
    }

    println!("cargo:rustc-link-lib=static=ambivox_engine");
    let target_os = env::var("CARGO_CFG_TARGET_OS").unwrap();
    let runtime = if target_os == "macos" || target_os == "ios" { "c++" } else { "stdc++" };
    println!("cargo:rustc-link-lib=dylib={runtime}");
}
