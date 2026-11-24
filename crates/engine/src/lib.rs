use std::ffi::{CStr, CString, c_char, c_int, c_void};
use std::fmt;
use std::path::Path;
use std::ptr::{self, NonNull};

pub const ASR_SAMPLE_RATE: u32 = 16_000;
pub const LANGUAGES: [&str; 4] = ["en", "fr", "es", "it"];

mod ffi {
    use super::*;

    #[repr(C)]
    pub struct Engine {
        _private: [u8; 0],
    }

    #[repr(C)]
    pub struct Config {
        pub backend: *const c_char,
        pub model_dir: *const c_char,
        pub threads: c_int,
        pub asr_model: *const c_char,
        pub translator: *const c_char,
    }

    unsafe extern "C" {
        pub fn ambivox_engine_open(config: *const Config) -> *mut Engine;
        pub fn ambivox_engine_close(engine: *mut Engine);
        pub fn ambivox_engine_backend(engine: *const Engine) -> *const c_char;
        pub fn ambivox_transcribe(
            engine: *mut Engine,
            pcm: *const f32,
            n: usize,
            lang: *const c_char,
            text: *mut *mut c_char,
        ) -> c_int;
        pub fn ambivox_translate(
            engine: *mut Engine,
            text: *const c_char,
            source: *const c_char,
            target: *const c_char,
            out: *mut *mut c_char,
        ) -> c_int;
        pub fn ambivox_translate_stream(
            engine: *mut Engine,
            text: *const c_char,
            source: *const c_char,
            target: *const c_char,
            on_text: Option<unsafe extern "C" fn(*mut c_void, *const c_char)>,
            user: *mut c_void,
            out: *mut *mut c_char,
        ) -> c_int;
        pub fn ambivox_synthesize(
            engine: *mut Engine,
            text: *const c_char,
            lang: *const c_char,
            pcm: *mut *mut f32,
            n: *mut usize,
            sample_rate: *mut c_int,
        ) -> c_int;
        pub fn ambivox_free(p: *mut c_void);
        pub fn ambivox_last_error() -> *const c_char;
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Error {
    Argument(String),
    Unsupported(String),
    Model(String),
    Inference(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Argument(m) => write!(f, "invalid argument: {m}"),
            Error::Unsupported(m) => write!(f, "unsupported: {m}"),
            Error::Model(m) => write!(f, "model error: {m}"),
            Error::Inference(m) => write!(f, "inference failed: {m}"),
        }
    }
}

impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, Clone, PartialEq)]
pub struct Audio {
    pub samples: Vec<f32>,
    pub sample_rate: u32,
}

pub struct Engine {
    raw: NonNull<ffi::Engine>,
}

// SAFETY: the C++ engine serialises access to every model behind its own mutexes
// and keeps error state in thread-local storage.
unsafe impl Send for Engine {}
unsafe impl Sync for Engine {}

#[derive(Clone, Copy, Debug)]
pub struct Options<'a> {
    pub model_dir: Option<&'a Path>,
    pub asr_model: Option<&'a str>,
    pub translator: Option<&'a str>,
    pub threads: usize,
}

impl Default for Options<'_> {
    fn default() -> Self {
        Self { model_dir: None, asr_model: None, translator: None, threads: 4 }
    }
}

impl Engine {
    pub fn open(backend: &str, options: &Options) -> Result<Self> {
        let backend = cstring(backend)?;
        let model_dir = match options.model_dir {
            Some(path) => Some(cstring(&path.to_string_lossy())?),
            None => None,
        };
        let asr_model = options.asr_model.map(cstring).transpose()?;
        let translator = options.translator.map(cstring).transpose()?;
        let config = ffi::Config {
            backend: backend.as_ptr(),
            model_dir: model_dir.as_ref().map_or(ptr::null(), |d| d.as_ptr()),
            threads: options.threads.min(c_int::MAX as usize) as c_int,
            asr_model: asr_model.as_ref().map_or(ptr::null(), |m| m.as_ptr()),
            translator: translator.as_ref().map_or(ptr::null(), |m| m.as_ptr()),
        };
        // SAFETY: `config` and the strings it points to outlive the call.
        let raw = unsafe { ffi::ambivox_engine_open(&config) };
        NonNull::new(raw).map(|raw| Self { raw }).ok_or_else(|| {
            let message = last_error();
            if message.contains("unknown backend") || message.contains("built without") {
                Error::Unsupported(message)
            } else {
                Error::Model(message)
            }
        })
    }

    pub fn sim() -> Self {
        Self::open("sim", &Options { threads: 1, ..Options::default() })
            .expect("the simulated backend is always available")
    }

    pub fn backend(&self) -> &str {
        // SAFETY: the engine returns a pointer to a static NUL-terminated string.
        unsafe { CStr::from_ptr(ffi::ambivox_engine_backend(self.raw.as_ptr())) }.to_str().unwrap_or("")
    }

    pub fn transcribe(&self, pcm: &[f32], lang: &str) -> Result<String> {
        let lang = cstring(lang)?;
        let mut out = ptr::null_mut();
        // SAFETY: `pcm` is valid for `pcm.len()` floats and `out` receives an engine-owned string.
        let status =
            unsafe { ffi::ambivox_transcribe(self.raw.as_ptr(), pcm.as_ptr(), pcm.len(), lang.as_ptr(), &mut out) };
        take_string(status, out)
    }

    pub fn translate(&self, text: &str, source: &str, target: &str) -> Result<String> {
        let (text, source, target) = (cstring(text)?, cstring(source)?, cstring(target)?);
        let mut out = ptr::null_mut();
        // SAFETY: all inputs are NUL-terminated and `out` receives an engine-owned string.
        let status = unsafe {
            ffi::ambivox_translate(self.raw.as_ptr(), text.as_ptr(), source.as_ptr(), target.as_ptr(), &mut out)
        };
        take_string(status, out)
    }

    pub fn translate_with<F: FnMut(&str)>(
        &self,
        text: &str,
        source: &str,
        target: &str,
        mut on_text: F,
    ) -> Result<String> {
        unsafe extern "C" fn grown<F: FnMut(&str)>(user: *mut c_void, text: *const c_char) {
            // SAFETY: `user` is the `F` passed below, alive for the whole call, and `text` is NUL-terminated.
            let (on_text, text) = unsafe { (&mut *user.cast::<F>(), CStr::from_ptr(text)) };
            if let Ok(text) = text.to_str() {
                on_text(text);
            }
        }
        let (text, source, target) = (cstring(text)?, cstring(source)?, cstring(target)?);
        let mut out = ptr::null_mut();
        // SAFETY: as in `translate`; the callback only runs during the call, on this thread.
        let status = unsafe {
            ffi::ambivox_translate_stream(
                self.raw.as_ptr(),
                text.as_ptr(),
                source.as_ptr(),
                target.as_ptr(),
                Some(grown::<F>),
                (&raw mut on_text).cast(),
                &mut out,
            )
        };
        take_string(status, out)
    }

    pub fn synthesize(&self, text: &str, lang: &str) -> Result<Audio> {
        let (text, lang) = (cstring(text)?, cstring(lang)?);
        let mut pcm = ptr::null_mut();
        let mut n = 0usize;
        let mut sample_rate: c_int = 0;
        // SAFETY: the out-pointers are valid; on success `pcm` holds `n` floats owned by the engine.
        let status = unsafe {
            ffi::ambivox_synthesize(self.raw.as_ptr(), text.as_ptr(), lang.as_ptr(), &mut pcm, &mut n, &mut sample_rate)
        };
        check(status)?;
        // SAFETY: see above; the buffer is released right after being copied.
        let samples = unsafe {
            let samples = std::slice::from_raw_parts(pcm, n).to_vec();
            ffi::ambivox_free(pcm.cast());
            samples
        };
        Ok(Audio { samples, sample_rate: sample_rate as u32 })
    }
}

impl Drop for Engine {
    fn drop(&mut self) {
        // SAFETY: `raw` came from `ambivox_engine_open` and is closed exactly once.
        unsafe { ffi::ambivox_engine_close(self.raw.as_ptr()) }
    }
}

fn cstring(s: &str) -> Result<CString> {
    CString::new(s).map_err(|_| Error::Argument("string contains a NUL byte".into()))
}

fn last_error() -> String {
    // SAFETY: the engine returns a NUL-terminated thread-local string.
    unsafe { CStr::from_ptr(ffi::ambivox_last_error()) }.to_string_lossy().into_owned()
}

fn check(status: c_int) -> Result<()> {
    match status {
        0 => Ok(()),
        1 => Err(Error::Argument(last_error())),
        2 => Err(Error::Unsupported(last_error())),
        3 => Err(Error::Model(last_error())),
        _ => Err(Error::Inference(last_error())),
    }
}

fn take_string(status: c_int, out: *mut c_char) -> Result<String> {
    check(status)?;
    // SAFETY: on success the engine hands over a NUL-terminated string that we free once.
    unsafe {
        let text = CStr::from_ptr(out).to_string_lossy().into_owned();
        ffi::ambivox_free(out.cast());
        Ok(text)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn decimate(audio: &Audio) -> Vec<f32> {
        let step = audio.sample_rate as f64 / ASR_SAMPLE_RATE as f64;
        let n = (audio.samples.len() as f64 / step) as usize;
        (0..n).map(|i| audio.samples[(i as f64 * step) as usize]).collect()
    }

    #[test]
    fn simulated_roundtrip() {
        let engine = Engine::sim();
        assert_eq!(engine.backend(), "sim");

        let audio = engine.synthesize("hello world", "en").unwrap();
        assert_eq!(audio.sample_rate, 22_050);
        let heard = engine.transcribe(&decimate(&audio), "en").unwrap();
        assert_eq!(heard, "hello world");
        assert_eq!(engine.translate(&heard, "en", "it").unwrap(), "ciao mondo");
    }

    #[test]
    fn translations_can_be_followed_as_they_grow() {
        let engine = Engine::sim();
        let mut grown = Vec::new();
        let translated = engine.translate_with("hello my friend", "en", "fr", |text| grown.push(text.to_string()));
        assert_eq!(translated.unwrap(), "bonjour my ami");
        assert_eq!(grown, ["bonjour", "bonjour my", "bonjour my ami"]);
        assert!(matches!(engine.translate_with("hello", "en", "de", |_| {}), Err(Error::Unsupported(_))));
    }

    #[test]
    fn errors_are_typed() {
        let engine = Engine::sim();
        assert!(matches!(engine.translate("hello", "en", "de"), Err(Error::Unsupported(m)) if m.contains("de")));
        assert!(matches!(engine.translate("a\0b", "en", "fr"), Err(Error::Argument(_))));
        assert!(matches!(Engine::open("nope", &Options::default()), Err(Error::Unsupported(_))));
    }

    #[test]
    fn silence_transcribes_to_nothing() {
        let engine = Engine::sim();
        assert_eq!(engine.transcribe(&[0.0; 16_000], "fr").unwrap(), "");
        assert_eq!(engine.transcribe(&[], "fr").unwrap(), "");
    }

    #[test]
    fn engine_is_shareable_across_threads() {
        let engine = std::sync::Arc::new(Engine::sim());
        let handles: Vec<_> = (0..4)
            .map(|i| {
                let engine = engine.clone();
                std::thread::spawn(move || {
                    let text = format!("voice {i}");
                    let audio = engine.synthesize(&text, "en").unwrap();
                    assert_eq!(engine.transcribe(&decimate(&audio), "en").unwrap(), text);
                })
            })
            .collect();
        handles.into_iter().for_each(|h| h.join().unwrap());
    }
}
