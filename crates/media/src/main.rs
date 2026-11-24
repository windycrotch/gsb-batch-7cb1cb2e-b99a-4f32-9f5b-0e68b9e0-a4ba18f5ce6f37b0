use std::collections::HashMap;
use std::fs::File;
use std::io::{BufReader, BufWriter, Write};
use std::os::unix::net::UnixListener;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::sync::Arc;

use ambivox_dsp::kernels;
use ambivox_dsp::resample::resample;
use ambivox_engine::{ASR_SAMPLE_RATE, Engine, Options};
use ambivox_media::codec::{self, Decoder, Encoder, SAMPLE_RATE};
use ambivox_media::pipeline::{Config, Pipeline};
use ambivox_media::{bench, log, server, wav};

const USAGE: &str = "\
ambivox-media — real-time speech translation engine

USAGE:
    ambivox-media serve  [--socket PATH] [--backend sim|sussurro] [--models DIR] [--profile NAME]
                         [--whisper NAME] [--translator NAME] [--workers N] [--threads N]
                         [--bitrate BPS] [--partials-ms MS] [--early-words N]
    ambivox-media synth  --text TEXT --lang LANG (--out FILE | --wav FILE)
                         [--backend ...] [--models DIR]
    ambivox-media listen --lang LANG (--in FILE | --wav FILE)
                         [--backend ...] [--models DIR] [--profile NAME] [--whisper NAME]
    ambivox-media bench  [--set FILE] [--recordings DIR] [--only PAIR] [--json FILE]
                         [--backend ...] [--models DIR] [--profile NAME] [--whisper NAME]
                         [--translator NAME] [--threads N]
    ambivox-media info

`synth` speaks TEXT and stores it as length-prefixed Opus packets (--out) or as
a 48 kHz WAV file (--wav). `listen` transcribes either format.
`--partials-ms` sets how often live captions are refreshed while someone is
speaking (default 800; 0 turns them off). `--profile` picks the models:
light (whisper base, opus), balanced (whisper small, opus; the default) or
quality (whisper large-v3-turbo, translategemma-12b). `--whisper` and
`--translator` override either: ggml-NAME.bin in the model directory for
recognition; opus for the per-pair translation models, or NAME for NAME.gguf
in the model directory, run with llama.cpp. `--threads` defaults to the cores
available divided by the workers, at most 8. `--early-words`
is how many settled words a stream opened in headphones mode speaks before the
sentence ends, when no comma comes first (default 8).
`bench` runs each sentence of a set (default bench/sentences.txt) through
recognition, translation and synthesis, then reports the word error rate, chrF
against the reference translation, and how long each step took. Each sentence is
spoken by the engine's own voice, unless --recordings holds a file named ID.wav.
";

type Failure = Box<dyn std::error::Error>;

const MAX_THREADS: usize = 8;

const PROFILES: [(&str, &str, &str); 3] =
    [("light", "base", "opus"), ("balanced", "small", "opus"), ("quality", "large-v3-turbo", "translategemma-12b")];

struct Args {
    values: HashMap<String, String>,
}

impl Args {
    fn parse(args: impl Iterator<Item = String>) -> Result<Self, Failure> {
        let mut values = HashMap::new();
        let mut args = args.peekable();
        while let Some(flag) = args.next() {
            let name = flag.strip_prefix("--").ok_or_else(|| format!("unexpected argument: {flag}"))?;
            let value = args.next().ok_or_else(|| format!("missing value for --{name}"))?;
            values.insert(name.to_string(), value);
        }
        Ok(Self { values })
    }

    fn get(&self, name: &str) -> Option<&str> {
        self.values.get(name).map(String::as_str)
    }

    fn require(&self, name: &str) -> Result<&str, Failure> {
        self.get(name).ok_or_else(|| format!("missing --{name}").into())
    }

    fn number<T: std::str::FromStr>(&self, name: &str, default: T) -> Result<T, Failure> {
        match self.get(name) {
            Some(value) => value.parse().map_err(|_| format!("invalid value for --{name}: {value}").into()),
            None => Ok(default),
        }
    }

    fn set(&self, name: &str) -> Option<&str> {
        self.get(name).filter(|value| !value.is_empty())
    }

    fn models(&self) -> Result<(&str, &str), Failure> {
        let profile = self.set("profile").unwrap_or("balanced");
        let (_, whisper, translator) = PROFILES
            .iter()
            .find(|(name, ..)| *name == profile)
            .ok_or_else(|| format!("unknown profile: {profile} (expected light, balanced or quality)"))?;
        Ok((self.set("whisper").unwrap_or(whisper), self.set("translator").unwrap_or(translator)))
    }

    fn threads(&self) -> Result<usize, Failure> {
        let workers = self.number("workers", 2usize)?.max(1);
        let cores = std::thread::available_parallelism().map_or(4, |n| n.get());
        self.number("threads", (cores / workers).clamp(1, MAX_THREADS))
    }

    fn engine(&self) -> Result<Engine, Failure> {
        let backend = self.get("backend").unwrap_or("sim");
        let (whisper, translator) = self.models()?;
        let options = Options {
            model_dir: self.get("models").map(Path::new),
            asr_model: Some(whisper),
            translator: Some(translator),
            threads: self.threads()?,
        };
        Ok(Engine::open(backend, &options)?)
    }
}

fn serve(args: &Args) -> Result<(), Failure> {
    let socket = PathBuf::from(args.get("socket").unwrap_or("/tmp/ambivox-media.sock"));
    let config = Config {
        workers: args.number("workers", 2)?,
        bitrate: args.number("bitrate", 32_000)?,
        partial_interval_ms: args.number("partials-ms", 800)?,
        early_words: args.number("early-words", 8)?,
        ..Config::default()
    };
    let engine = Arc::new(args.engine()?);
    if engine.backend() != "sim" {
        let (whisper, translator) = args.models()?;
        log::info(&format!("models: whisper {whisper}, translator {translator}, {} threads", args.threads()?));
    }
    engine
        .transcribe(&vec![0.0; 2 * ASR_SAMPLE_RATE as usize], "en")
        .map_err(|e| format!("speech recognition self-test failed: {e}"))?;

    if socket.exists() {
        std::fs::remove_file(&socket)?;
    }
    let listener = UnixListener::bind(&socket)?;
    log::info(&format!(
        "listening on {} (backend {}, {} workers, {} kernels, live captions every {} ms)",
        socket.display(),
        engine.backend(),
        config.workers,
        kernels::backend(),
        config.partial_interval_ms
    ));
    server::serve(listener, Pipeline::new(engine, config))?;
    Ok(())
}

fn synth(args: &Args) -> Result<(), Failure> {
    let engine = args.engine()?;
    let audio = engine.synthesize(args.require("text")?, args.require("lang")?)?;
    let mut pcm = vec![0.0; SAMPLE_RATE as usize / 2];
    pcm.extend(resample(&audio.samples, audio.sample_rate, SAMPLE_RATE));
    pcm.resize(pcm.len() + SAMPLE_RATE as usize, 0.0);

    match (args.get("out"), args.get("wav")) {
        (Some(path), None) => {
            let packets = Encoder::new(args.number("bitrate", 32_000)?)?.encode_all(&pcm)?;
            let mut file = BufWriter::new(File::create(path)?);
            codec::write_packets(&mut file, &packets)?;
            file.flush()?;
        }
        (None, Some(path)) => {
            let mut file = BufWriter::new(File::create(path)?);
            wav::write(&mut file, &pcm, SAMPLE_RATE)?;
            file.flush()?;
        }
        _ => return Err("give exactly one of --out or --wav".into()),
    }
    Ok(())
}

fn listen(args: &Args) -> Result<(), Failure> {
    let engine = args.engine()?;
    let (pcm, rate) = match (args.get("in"), args.get("wav")) {
        (Some(path), None) => {
            let mut decoder = Decoder::new()?;
            let mut pcm = Vec::new();
            for packet in codec::read_packets(&mut BufReader::new(File::open(path)?))? {
                if packet.is_empty() || decoder.decode(&packet, &mut pcm).is_err() {
                    decoder.conceal(&mut pcm)?;
                }
            }
            (pcm, SAMPLE_RATE)
        }
        (None, Some(path)) => wav::read(&mut BufReader::new(File::open(path)?))?,
        _ => return Err("give exactly one of --in or --wav".into()),
    };
    println!("{}", engine.transcribe(&resample(&pcm, rate, ASR_SAMPLE_RATE), args.require("lang")?)?);
    Ok(())
}

fn run_bench(args: &Args) -> Result<(), Failure> {
    let path = args.get("set").unwrap_or("bench/sentences.txt");
    let set = std::fs::read_to_string(path).map_err(|e| format!("{path}: {e}"))?;
    let mut cases = bench::parse_set(&set).map_err(|e| format!("{path}: {e}"))?;
    if let Some(pair) = args.get("only") {
        cases.retain(|case| format!("{}-{}", case.source, case.target) == pair);
    }
    if cases.is_empty() {
        return Err("no sentences to run".into());
    }
    let recordings = args.get("recordings").map(Path::new);
    let engine = args.engine()?;
    let (whisper, translator) = if engine.backend() == "sim" { ("-", "-") } else { args.models()? };

    eprintln!("warming up {} ({} sentences)", engine.backend(), cases.len());
    let skipped = bench::warm_up(&engine, &mut cases);
    if cases.is_empty() {
        return Err(format!("every language pair failed:\n{}", skipped.join("\n")).into());
    }
    let mut outcomes = Vec::with_capacity(cases.len());
    for (i, case) in cases.iter().enumerate() {
        eprint!("\r{}/{}", i + 1, cases.len());
        outcomes.push(bench::measure(&engine, case, recordings)?);
    }
    eprintln!();

    let recorded = outcomes.iter().filter(|o| o.recorded).count();
    let rows = bench::summaries(&outcomes);
    println!(
        "backend {}, whisper {}, translator {}, {} sentences, {} recorded, {} synthesized\n",
        engine.backend(),
        whisper,
        translator,
        outcomes.len(),
        recorded,
        outcomes.len() - recorded
    );
    for pair in &skipped {
        println!("skipped {pair}");
    }
    if !skipped.is_empty() {
        println!();
    }
    print!("{}", bench::table(&rows));
    let misheard = bench::misheard(&outcomes);
    if !misheard.is_empty() {
        println!("\nmisheard:\n{misheard}");
    }
    let mistranslated = bench::mistranslated(&outcomes);
    if !mistranslated.is_empty() {
        println!("\nfar from the reference:\n{mistranslated}");
    }
    if let Some(path) = args.get("json") {
        std::fs::write(path, bench::json(engine.backend(), whisper, translator, &rows, &outcomes))?;
    }
    Ok(())
}

fn info() {
    println!("ambivox-media {}", env!("CARGO_PKG_VERSION"));
    println!("kernels: {}", kernels::backend());
    let sussurro = if cfg!(feature = "sussurro") { ", sussurro" } else { "" };
    println!("backends: sim{sussurro}");
}

fn main() -> ExitCode {
    let mut argv = std::env::args().skip(1);
    let command = argv.next().unwrap_or_default();
    let result = match command.as_str() {
        "serve" | "synth" | "listen" | "bench" => Args::parse(argv).and_then(|args| match command.as_str() {
            "serve" => serve(&args),
            "synth" => synth(&args),
            "bench" => run_bench(&args),
            _ => listen(&args),
        }),
        "info" => {
            info();
            Ok(())
        }
        "" | "help" | "--help" | "-h" => {
            print!("{USAGE}");
            Ok(())
        }
        other => Err(format!("unknown command: {other}\n\n{USAGE}").into()),
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("ambivox-media: {error}");
            ExitCode::FAILURE
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Args {
        Args::parse(list.iter().map(|s| s.to_string())).unwrap()
    }

    #[test]
    fn profiles_pick_the_models_and_flags_override_them() {
        assert_eq!(args(&[]).models().unwrap(), ("small", "opus"));
        assert_eq!(args(&["--profile", "quality"]).models().unwrap(), ("large-v3-turbo", "translategemma-12b"));
        assert_eq!(args(&["--profile", "light", "--whisper", "small"]).models().unwrap(), ("small", "opus"));
        assert_eq!(args(&["--profile", "", "--translator", ""]).models().unwrap(), ("small", "opus"));
        assert!(args(&["--profile", "huge"]).models().is_err());
    }

    #[test]
    fn threads_share_the_cores_between_workers() {
        let cores = std::thread::available_parallelism().unwrap().get();
        assert_eq!(args(&["--threads", "3"]).threads().unwrap(), 3);
        assert_eq!(args(&["--workers", "1"]).threads().unwrap(), cores.clamp(1, MAX_THREADS));
        assert_eq!(args(&["--workers", "999"]).threads().unwrap(), 1);
        assert!(args(&["--threads", "many"]).threads().is_err());
    }

    #[test]
    fn the_setup_script_knows_the_same_profiles() {
        let script = include_str!("../../../scripts/setup-sussurro.sh");
        for (name, whisper, translator) in PROFILES {
            let line = format!("{name}) WHISPER={whisper} TRANSLATOR={translator} ;;");
            assert!(script.contains(&line), "scripts/setup-sussurro.sh lacks: {line}");
        }
    }
}
