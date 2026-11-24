use std::collections::VecDeque;
use std::fmt;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender, SyncSender, TrySendError};
use std::sync::{Arc, Condvar, Mutex};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

use ambivox_dsp::resample::resample;
use ambivox_dsp::{Resampler, Segmenter, SegmenterConfig};
use ambivox_engine::{ASR_SAMPLE_RATE, Engine, LANGUAGES};

use crate::codec::{Decoder, Encoder, FRAME_MS, FRAME_SAMPLES, SAMPLE_RATE};
use crate::protocol::{CaptionKind, Frame};

const INPUT_QUEUE: usize = 256;
const MAX_CONCEALED: u16 = 10;
const IDLE: Duration = Duration::from_millis(100);
const STALL_GRACE: u32 = 4;
const UTTERANCE_GAP_MS: usize = 150;
const MIN_PARTIAL_MS: usize = 600;
const CLAUSE_WORDS: usize = 3;
const CHUNK_GAP_MS: usize = 80;

#[derive(Clone, Debug)]
pub struct Config {
    pub workers: usize,
    pub realtime: bool,
    pub bitrate: i32,
    pub max_backlog_ms: usize,
    pub partial_interval_ms: usize,
    pub early_words: usize,
    pub segmenter: SegmenterConfig,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            workers: 2,
            realtime: true,
            bitrate: 32_000,
            max_backlog_ms: 20_000,
            partial_interval_ms: 800,
            early_words: 8,
            segmenter: SegmenterConfig::default(),
        }
    }
}

#[derive(Default)]
pub struct Stats {
    pub streams: AtomicI64,
    pub packets_in: AtomicU64,
    pub packets_lost: AtomicU64,
    pub packets_dropped: AtomicU64,
    pub utterances: AtomicU64,
    pub partials: AtomicU64,
    pub partials_skipped: AtomicU64,
    pub frames_out: AtomicU64,
    pub frames_dropped: AtomicU64,
    pub backlog_dropped_ms: AtomicU64,
    pub errors: AtomicU64,
    pub inference_ms: AtomicU64,
    pub partial_ms: AtomicU64,
    pub early_chunks: AtomicU64,
    pub streamed_clauses: AtomicU64,
}

impl Stats {
    pub fn to_json(&self) -> String {
        format!(
            concat!(
                "{{\"streams\":{},\"packets_in\":{},\"packets_lost\":{},\"packets_dropped\":{},",
                "\"utterances\":{},\"partials\":{},\"partials_skipped\":{},\"frames_out\":{},",
                "\"frames_dropped\":{},\"backlog_dropped_ms\":{},\"errors\":{},\"inference_ms\":{},",
                "\"partial_ms\":{},\"early_chunks\":{},\"streamed_clauses\":{}}}"
            ),
            self.streams.load(Ordering::Relaxed),
            self.packets_in.load(Ordering::Relaxed),
            self.packets_lost.load(Ordering::Relaxed),
            self.packets_dropped.load(Ordering::Relaxed),
            self.utterances.load(Ordering::Relaxed),
            self.partials.load(Ordering::Relaxed),
            self.partials_skipped.load(Ordering::Relaxed),
            self.frames_out.load(Ordering::Relaxed),
            self.frames_dropped.load(Ordering::Relaxed),
            self.backlog_dropped_ms.load(Ordering::Relaxed),
            self.errors.load(Ordering::Relaxed),
            self.inference_ms.load(Ordering::Relaxed),
            self.partial_ms.load(Ordering::Relaxed),
            self.early_chunks.load(Ordering::Relaxed),
            self.streamed_clauses.load(Ordering::Relaxed),
        )
    }
}

#[derive(Debug)]
pub enum OpenError {
    UnsupportedLanguage(String),
    Codec(String),
}

impl fmt::Display for OpenError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            OpenError::UnsupportedLanguage(lang) => write!(f, "unsupported language: {lang}"),
            OpenError::Codec(message) => write!(f, "{message}"),
        }
    }
}

impl std::error::Error for OpenError {}

pub type Sink = SyncSender<Frame>;

enum Input {
    Packet { sequence: u16, data: Vec<u8> },
    Close,
}

struct Job {
    index: u32,
    pcm: Vec<f32>,
    partial: bool,
}

#[derive(Default)]
struct Early {
    index: u32,
    previous: Vec<String>,
    committed: Vec<String>,
}

impl Early {
    fn reset(&mut self, index: u32) {
        if self.index != index {
            *self = Early { index, ..Early::default() };
        }
    }

    fn advance(&mut self, heard: &str, run: usize) -> Option<String> {
        let words: Vec<&str> = heard.split_whitespace().collect();
        let current: Vec<String> = words.iter().map(|w| normalize(w)).collect();
        let previous = std::mem::replace(&mut self.previous, current);
        let done = self.committed.len();
        if common_prefix(&self.committed, &self.previous) < done {
            return None;
        }
        let agreed = common_prefix(&previous, &self.previous).min(words.len().saturating_sub(1));
        if agreed <= done {
            return None;
        }
        let clause = (done + CLAUSE_WORDS..=agreed).rev().find(|&end| ends_clause(words[end - 1]));
        let end = clause.or((agreed - done >= run.max(1)).then_some(agreed))?;
        self.committed = self.previous[..end].to_vec();
        Some(words[done..end].join(" "))
    }

    fn remainder<'a>(&self, heard: &'a str) -> &'a str {
        let mut rest = heard.trim_start();
        for _ in 0..self.committed.len() {
            rest = match rest.split_once(char::is_whitespace) {
                Some((_, tail)) => tail.trim_start(),
                None => "",
            };
        }
        rest
    }
}

fn normalize(word: &str) -> String {
    word.chars().filter(|c| c.is_alphanumeric()).flat_map(char::to_lowercase).collect()
}

fn common_prefix(a: &[String], b: &[String]) -> usize {
    a.iter().zip(b).take_while(|(x, y)| x == y).count()
}

fn ends_clause(word: &str) -> bool {
    word.ends_with([',', '.', ';', ':', '?', '!'])
}

pub fn clause_end(text: &str) -> Option<usize> {
    let (mut words, mut start) = (0, None);
    for (i, c) in text.char_indices() {
        if !c.is_whitespace() {
            start.get_or_insert(i);
        } else if let Some(begin) = start.take() {
            words += 1;
            if words >= CLAUSE_WORDS && ends_clause(&text[begin..i]) {
                return Some(i);
            }
        }
    }
    None
}

#[derive(Default)]
struct Playout {
    samples: VecDeque<f32>,
    pending_jobs: usize,
    input_done: bool,
}

struct Shared {
    id: u32,
    source: String,
    target: String,
    sink: Sink,
    jobs: Mutex<VecDeque<Job>>,
    scheduled: AtomicBool,
    partial_pending: AtomicBool,
    last_partial: Mutex<(u32, String)>,
    early: Option<Mutex<Early>>,
    playout: Mutex<Playout>,
    playout_changed: Condvar,
    closed: AtomicBool,
}

impl Shared {
    fn emit(&self, frame: Frame, stats: &Stats) {
        if let Err(TrySendError::Full(_)) = self.sink.try_send(frame) {
            stats.frames_dropped.fetch_add(1, Ordering::Relaxed);
        }
    }

    fn is_closed(&self) -> bool {
        self.closed.load(Ordering::Acquire)
    }
}

pub struct Pipeline {
    engine: Arc<Engine>,
    config: Config,
    stats: Arc<Stats>,
    run_queue: Sender<Arc<Shared>>,
}

impl Pipeline {
    pub fn new(engine: Arc<Engine>, config: Config) -> Arc<Self> {
        let (run_queue, jobs) = mpsc::channel::<Arc<Shared>>();
        let jobs = Arc::new(Mutex::new(jobs));
        let stats = Arc::new(Stats::default());
        let pipeline = Arc::new(Self { engine, config, stats, run_queue });

        for i in 0..pipeline.config.workers.max(1) {
            let (jobs, pipeline) = (jobs.clone(), Arc::downgrade(&pipeline));
            thread::Builder::new()
                .name(format!("inference-{i}"))
                .spawn(move || {
                    loop {
                        let next = jobs.lock().unwrap().recv();
                        let (Ok(shared), Some(pipeline)) = (next, pipeline.upgrade()) else { break };
                        pipeline.work(&shared);
                    }
                })
                .expect("cannot spawn inference worker");
        }
        pipeline
    }

    pub fn stats(&self) -> &Arc<Stats> {
        &self.stats
    }

    pub fn backend(&self) -> &str {
        self.engine.backend()
    }

    pub fn open(
        self: &Arc<Self>,
        id: u32,
        source: &str,
        target: &str,
        early: bool,
        sink: Sink,
    ) -> Result<Stream, OpenError> {
        for lang in [source, target] {
            if !LANGUAGES.contains(&lang) {
                return Err(OpenError::UnsupportedLanguage(lang.to_string()));
            }
        }
        let decoder = Decoder::new().map_err(|e| OpenError::Codec(e.to_string()))?;
        let encoder = Encoder::new(self.config.bitrate).map_err(|e| OpenError::Codec(e.to_string()))?;

        let shared = Arc::new(Shared {
            id,
            source: source.to_string(),
            target: target.to_string(),
            sink,
            jobs: Mutex::new(VecDeque::new()),
            scheduled: AtomicBool::new(false),
            partial_pending: AtomicBool::new(false),
            last_partial: Mutex::new((u32::MAX, String::new())),
            early: early.then(|| Mutex::new(Early::default())),
            playout: Mutex::new(Playout::default()),
            playout_changed: Condvar::new(),
            closed: AtomicBool::new(false),
        });
        let (input, packets) = mpsc::sync_channel(INPUT_QUEUE);

        let listener = {
            let (pipeline, shared) = (self.clone(), shared.clone());
            thread::Builder::new()
                .name(format!("listen-{id}"))
                .spawn(move || pipeline.listen(&shared, packets, decoder))
                .expect("cannot spawn stream thread")
        };
        let speaker = {
            let (pipeline, shared) = (self.clone(), shared.clone());
            thread::Builder::new()
                .name(format!("speak-{id}"))
                .spawn(move || pipeline.speak(&shared, encoder))
                .expect("cannot spawn stream thread")
        };

        self.stats.streams.fetch_add(1, Ordering::Relaxed);
        Ok(Stream { input, shared, stats: self.stats.clone(), threads: vec![listener, speaker] })
    }

    fn listen(&self, shared: &Arc<Shared>, packets: Receiver<Input>, mut decoder: Decoder) {
        let mut resampler = Resampler::new(SAMPLE_RATE, ASR_SAMPLE_RATE);
        let mut segmenter = Segmenter::new(self.config.segmenter.clone());
        let mut expected: Option<u16> = None;
        let mut next_index = 0u32;
        let mut idle = 0u32;
        let mut partial_at = 0usize;
        let mut finished = Vec::new();
        let mut wideband = Vec::with_capacity(FRAME_SAMPLES * 4);
        let mut narrowband = Vec::with_capacity(FRAME_SAMPLES);
        let silence = vec![0.0f32; ASR_SAMPLE_RATE as usize * IDLE.as_millis() as usize / 1000];
        let samples = |ms: usize| ASR_SAMPLE_RATE as usize * ms / 1000;
        let (min_partial, partial_step) = (samples(MIN_PARTIAL_MS), samples(self.config.partial_interval_ms));

        while !shared.is_closed() {
            match packets.recv_timeout(IDLE) {
                Ok(Input::Packet { sequence, data }) => {
                    idle = 0;
                    self.stats.packets_in.fetch_add(1, Ordering::Relaxed);
                    wideband.clear();
                    if let Some(expected) = expected {
                        let gap = sequence.wrapping_sub(expected);
                        if gap >= 0x8000 {
                            self.stats.packets_dropped.fetch_add(1, Ordering::Relaxed);
                            continue;
                        }
                        self.stats.packets_lost.fetch_add(gap as u64, Ordering::Relaxed);
                        for _ in 0..gap.min(MAX_CONCEALED) {
                            let _ = decoder.conceal(&mut wideband);
                        }
                    }
                    expected = Some(sequence.wrapping_add(1));
                    if decoder.decode(&data, &mut wideband).is_err() {
                        self.stats.errors.fetch_add(1, Ordering::Relaxed);
                    }
                    narrowband.clear();
                    resampler.process(&wideband, &mut narrowband);
                    segmenter.push(&narrowband, |u| finished.push(u));
                }
                Err(RecvTimeoutError::Timeout) => {
                    idle += 1;
                    if idle > STALL_GRACE && segmenter.is_active() {
                        segmenter.push(&silence, |u| finished.push(u));
                    }
                }
                Ok(Input::Close) | Err(RecvTimeoutError::Disconnected) => break,
            }

            for pcm in finished.drain(..) {
                self.submit(shared, Job { index: next_index, pcm, partial: false });
                next_index += 1;
                partial_at = 0;
            }
            if partial_step == 0 {
                continue;
            }
            if let Some(open) = segmenter.open_utterance() {
                let due = open.len() >= min_partial.max(partial_at + partial_step);
                if due && !shared.partial_pending.swap(true, Ordering::AcqRel) {
                    partial_at = open.len();
                    self.submit(shared, Job { index: next_index, pcm: open.to_vec(), partial: true });
                }
            }
        }

        if !shared.is_closed() {
            segmenter.flush(|u| finished.push(u));
            for pcm in finished.drain(..) {
                self.submit(shared, Job { index: next_index, pcm, partial: false });
                next_index += 1;
            }
        }
        shared.playout.lock().unwrap().input_done = true;
        shared.playout_changed.notify_all();
    }

    fn submit(&self, shared: &Arc<Shared>, job: Job) {
        let mut jobs = shared.jobs.lock().unwrap();
        if job.partial {
            self.stats.partials.fetch_add(1, Ordering::Relaxed);
        } else {
            self.stats.utterances.fetch_add(1, Ordering::Relaxed);
            let before = jobs.len();
            jobs.retain(|queued| !queued.partial);
            if jobs.len() != before {
                self.stats.partials_skipped.fetch_add((before - jobs.len()) as u64, Ordering::Relaxed);
                shared.partial_pending.store(false, Ordering::Release);
            }
            shared.playout.lock().unwrap().pending_jobs += 1;
        }
        jobs.push_back(job);
        drop(jobs);
        self.schedule(shared);
    }

    fn schedule(&self, shared: &Arc<Shared>) {
        if !shared.scheduled.swap(true, Ordering::AcqRel) {
            let _ = self.run_queue.send(shared.clone());
        }
    }

    fn work(&self, shared: &Arc<Shared>) {
        let job = shared.jobs.lock().unwrap().pop_front();
        match job {
            Some(job) if job.partial => {
                if !shared.is_closed() {
                    self.preview(shared, job);
                }
                shared.partial_pending.store(false, Ordering::Release);
            }
            Some(job) => {
                if !shared.is_closed() {
                    self.interpret(shared, job);
                }
                shared.playout.lock().unwrap().pending_jobs -= 1;
                shared.playout_changed.notify_all();
            }
            None => {}
        }
        shared.scheduled.store(false, Ordering::Release);
        if !shared.jobs.lock().unwrap().is_empty() {
            self.schedule(shared);
        }
    }

    fn preview(&self, shared: &Shared, job: Job) {
        let started = Instant::now();
        let result = (|| {
            let heard = self.engine.transcribe(&job.pcm, &shared.source)?;
            let heard = heard.trim();
            if heard.is_empty() {
                return Ok(());
            }
            let chunk = shared.early.as_ref().and_then(|early| {
                let mut early = early.lock().unwrap();
                early.reset(job.index);
                early.advance(heard, self.config.early_words)
            });
            if let Some(chunk) = chunk {
                let translated = self.engine.translate(&chunk, &shared.source, &shared.target)?;
                if !translated.trim().is_empty() {
                    self.say(shared, translated.trim(), CHUNK_GAP_MS)?;
                    self.stats.early_chunks.fetch_add(1, Ordering::Relaxed);
                }
            }
            {
                let mut last = shared.last_partial.lock().unwrap();
                if last.0 == job.index && last.1 == heard {
                    return Ok(());
                }
                *last = (job.index, heard.to_string());
            }
            let caption = |kind, text: &str| {
                let frame = Frame::Caption { stream: shared.id, kind, utterance: job.index, text: text.to_string() };
                shared.emit(frame, &self.stats);
            };
            caption(CaptionKind::SourcePartial, heard);
            let translated = self.engine.translate(heard, &shared.source, &shared.target)?;
            if !translated.trim().is_empty() {
                caption(CaptionKind::TranslationPartial, translated.trim());
            }
            Ok::<(), ambivox_engine::Error>(())
        })();
        self.stats.partial_ms.fetch_add(started.elapsed().as_millis() as u64, Ordering::Relaxed);
        if result.is_err() {
            self.stats.errors.fetch_add(1, Ordering::Relaxed);
        }
    }

    fn interpret(&self, shared: &Shared, job: Job) {
        let started = Instant::now();
        let caption = |kind, text: &str| {
            let frame = Frame::Caption { stream: shared.id, kind, utterance: job.index, text: text.to_string() };
            shared.emit(frame, &self.stats);
        };

        let result = (|| {
            let heard = self.engine.transcribe(&job.pcm, &shared.source)?;
            let heard = heard.trim();
            if heard.is_empty() {
                return Ok(());
            }
            caption(CaptionKind::Source, heard);

            let rest = shared.early.as_ref().map_or(heard, |early| {
                let mut early = early.lock().unwrap();
                early.reset(job.index);
                let rest = early.remainder(heard);
                early.reset(job.index.wrapping_add(1));
                rest
            });
            if rest == heard {
                return self.interpret_aloud(shared, heard, &caption);
            }

            let translated = self.engine.translate(heard, &shared.source, &shared.target)?;
            if translated.trim().is_empty() {
                return Ok(());
            }
            caption(CaptionKind::Translation, translated.trim());
            if rest.is_empty() {
                return Ok(());
            }
            match self.engine.translate(rest, &shared.source, &shared.target)?.trim() {
                "" => Ok(()),
                rest => self.say(shared, rest, UTTERANCE_GAP_MS),
            }
        })();
        self.stats.inference_ms.fetch_add(started.elapsed().as_millis() as u64, Ordering::Relaxed);

        if let Err(error) = result {
            self.stats.errors.fetch_add(1, Ordering::Relaxed);
            shared.emit(Frame::Error { stream: shared.id, message: error.to_string() }, &self.stats);
        }
    }

    fn interpret_aloud(
        &self,
        shared: &Shared,
        heard: &str,
        caption: &dyn Fn(CaptionKind, &str),
    ) -> Result<(), ambivox_engine::Error> {
        thread::scope(|scope| {
            let (clauses, said) = mpsc::channel::<(String, usize)>();
            let voice = scope.spawn(move || {
                said.into_iter().try_for_each(|(text, gap): (String, usize)| self.say(shared, &text, gap))
            });

            let (mut so_far, mut spoken) = (String::new(), 0);
            let translated = self.engine.translate_with(heard, &shared.source, &shared.target, |text| {
                while let Some(end) = text.get(spoken..).and_then(clause_end) {
                    let _ = clauses.send((text[spoken..spoken + end].trim().to_string(), CHUNK_GAP_MS));
                    self.stats.streamed_clauses.fetch_add(1, Ordering::Relaxed);
                    spoken += end;
                }
                so_far.replace_range(.., text);
            });
            let translated = match translated {
                Ok(translated) => translated,
                Err(error) => {
                    drop(clauses);
                    let _ = voice.join();
                    return Err(error);
                }
            };
            if !translated.trim().is_empty() {
                caption(CaptionKind::Translation, translated.trim());
            }
            let rest = if spoken == 0 { translated.trim() } else { so_far.get(spoken..).unwrap_or("").trim() };
            if !rest.is_empty() {
                let _ = clauses.send((rest.to_string(), UTTERANCE_GAP_MS));
            }
            drop(clauses);
            voice.join().unwrap_or(Ok(()))
        })
    }

    fn say(&self, shared: &Shared, text: &str, gap_ms: usize) -> Result<(), ambivox_engine::Error> {
        let audio = self.engine.synthesize(text, &shared.target)?;
        let mut speech = resample(&audio.samples, audio.sample_rate, SAMPLE_RATE);
        speech.resize(speech.len() + SAMPLE_RATE as usize * gap_ms / 1000, 0.0);
        self.enqueue(shared, &speech);
        Ok(())
    }

    fn enqueue(&self, shared: &Shared, speech: &[f32]) {
        let limit = SAMPLE_RATE as usize * self.config.max_backlog_ms / 1000;
        let mut playout = shared.playout.lock().unwrap();
        playout.samples.extend(speech);
        if playout.samples.len() > limit {
            let excess = playout.samples.len() - limit;
            playout.samples.drain(..excess);
            let dropped_ms = excess as u64 * 1000 / SAMPLE_RATE as u64;
            self.stats.backlog_dropped_ms.fetch_add(dropped_ms, Ordering::Relaxed);
        }
        drop(playout);
        shared.playout_changed.notify_all();
    }

    fn speak(&self, shared: &Shared, mut encoder: Encoder) {
        let mut frame = [0.0f32; FRAME_SAMPLES];
        loop {
            {
                let mut playout = shared.playout.lock().unwrap();
                loop {
                    if shared.is_closed() {
                        return;
                    }
                    if !playout.samples.is_empty() {
                        break;
                    }
                    if playout.input_done && playout.pending_jobs == 0 {
                        drop(playout);
                        let _ = shared.sink.send(Frame::Closed { stream: shared.id });
                        return;
                    }
                    playout = shared.playout_changed.wait_timeout(playout, IDLE).unwrap().0;
                }
            }

            let mut deadline = Instant::now();
            loop {
                let taken = {
                    let mut playout = shared.playout.lock().unwrap();
                    let n = playout.samples.len().min(FRAME_SAMPLES);
                    for (slot, sample) in frame.iter_mut().zip(playout.samples.drain(..n)) {
                        *slot = sample;
                    }
                    n
                };
                if taken == 0 || shared.is_closed() {
                    break;
                }
                frame[taken..].fill(0.0);
                match encoder.encode(&frame) {
                    Ok(packet) => {
                        self.stats.frames_out.fetch_add(1, Ordering::Relaxed);
                        shared.emit(Frame::Speech { stream: shared.id, packet }, &self.stats);
                    }
                    Err(_) => {
                        self.stats.errors.fetch_add(1, Ordering::Relaxed);
                    }
                }
                if self.config.realtime {
                    deadline += Duration::from_millis(FRAME_MS);
                    thread::sleep(deadline.saturating_duration_since(Instant::now()));
                }
            }
        }
    }
}

pub struct Stream {
    input: SyncSender<Input>,
    shared: Arc<Shared>,
    stats: Arc<Stats>,
    threads: Vec<JoinHandle<()>>,
}

impl Stream {
    pub fn id(&self) -> u32 {
        self.shared.id
    }

    pub fn push(&self, sequence: u16, packet: Vec<u8>) -> bool {
        let accepted = self.input.try_send(Input::Packet { sequence, data: packet }).is_ok();
        if !accepted {
            self.stats.packets_dropped.fetch_add(1, Ordering::Relaxed);
        }
        accepted
    }

    pub fn close(mut self) {
        let _ = self.input.send(Input::Close);
        for thread in self.threads.drain(..) {
            let _ = thread.join();
        }
    }
}

impl Drop for Stream {
    fn drop(&mut self) {
        self.shared.closed.store(true, Ordering::Release);
        self.shared.playout_changed.notify_all();
        self.stats.streams.fetch_sub(1, Ordering::Relaxed);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::codec;

    fn pipeline(realtime: bool) -> Arc<Pipeline> {
        Pipeline::new(Arc::new(Engine::sim()), Config { realtime, ..Config::default() })
    }

    fn spoken(engine: &Engine, text: &str, lang: &str) -> Vec<Vec<u8>> {
        let audio = engine.synthesize(text, lang).unwrap();
        let mut pcm = vec![0.0; SAMPLE_RATE as usize / 2];
        pcm.extend(resample(&audio.samples, audio.sample_rate, SAMPLE_RATE));
        pcm.resize(pcm.len() + SAMPLE_RATE as usize, 0.0);
        Encoder::new(32_000).unwrap().encode_all(&pcm).unwrap()
    }

    struct Heard {
        captions: Vec<(CaptionKind, u32, String)>,
        partials: Vec<(CaptionKind, u32, String)>,
        errors: Vec<String>,
        packets: Vec<Vec<u8>>,
        closed: bool,
    }

    fn collect(frames: Receiver<Frame>) -> Heard {
        let mut heard = Heard { captions: vec![], partials: vec![], errors: vec![], packets: vec![], closed: false };
        while let Ok(frame) = frames.recv_timeout(Duration::from_secs(20)) {
            match frame {
                Frame::Caption { kind, utterance, text, .. } if kind.is_partial() => {
                    heard.partials.push((kind, utterance, text))
                }
                Frame::Caption { kind, utterance, text, .. } => heard.captions.push((kind, utterance, text)),
                Frame::Speech { packet, .. } => heard.packets.push(packet),
                Frame::Error { message, .. } => heard.errors.push(message),
                Frame::Closed { .. } => {
                    heard.closed = true;
                    break;
                }
                other => panic!("unexpected frame {other:?}"),
            }
        }
        heard
    }

    fn understand(engine: &Engine, packets: &[Vec<u8>], lang: &str) -> String {
        let mut decoder = Decoder::new().unwrap();
        let mut pcm = Vec::new();
        for packet in packets {
            decoder.decode(packet, &mut pcm).unwrap();
        }
        engine.transcribe(&resample(&pcm, SAMPLE_RATE, ASR_SAMPLE_RATE), lang).unwrap()
    }

    #[test]
    fn speech_comes_out_translated() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(1, "en", "fr", false, sink).unwrap();
        for (i, packet) in spoken(&engine, "hello world", "en").into_iter().enumerate() {
            assert!(stream.push(i as u16, packet));
            if i % 64 == 63 {
                thread::sleep(Duration::from_millis(20));
            }
        }
        stream.close();

        let heard = collect(frames);
        assert!(heard.closed);
        assert!(heard.errors.is_empty(), "{:?}", heard.errors);
        assert_eq!(
            heard.captions,
            vec![
                (CaptionKind::Source, 0, "hello world".to_string()),
                (CaptionKind::Translation, 0, "bonjour monde".to_string()),
            ]
        );
        assert_eq!(understand(&engine, &heard.packets, "fr"), "bonjour monde");
        assert_eq!(pipeline.stats().utterances.load(Ordering::Relaxed), 1);
        assert_eq!(pipeline.stats().streams.load(Ordering::Relaxed), 0);
    }

    fn play(stream: &Stream, packets: Vec<Vec<u8>>, realtime: bool) {
        let mut deadline = Instant::now();
        for (i, packet) in packets.into_iter().enumerate() {
            while !stream.push(i as u16, packet.clone()) {
                thread::sleep(Duration::from_millis(5));
            }
            if realtime {
                deadline += Duration::from_millis(FRAME_MS);
                thread::sleep(deadline.saturating_duration_since(Instant::now()));
            }
        }
    }

    #[test]
    fn captions_grow_while_the_speaker_talks() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(9, "en", "it", false, sink).unwrap();
        play(&stream, spoken(&engine, "thanks my friend and the sea", "en"), true);
        stream.close();

        let heard = collect(frames);
        let sources: Vec<&str> = heard
            .partials
            .iter()
            .filter(|(kind, ..)| *kind == CaptionKind::SourcePartial)
            .map(|(_, _, text)| text.as_str())
            .collect();
        assert!(sources.len() >= 3, "partials: {sources:?}");
        for pair in sources.windows(2) {
            assert!(pair[1].starts_with(pair[0]) && pair[1] != pair[0], "partials: {sources:?}");
        }
        assert!("thanks my friend and the sea".starts_with(sources[0]));
        assert!(heard.partials.iter().all(|(_, index, _)| *index == 0));
        assert!(
            heard
                .partials
                .iter()
                .any(|(kind, _, text)| *kind == CaptionKind::TranslationPartial && text.starts_with("grazie"))
        );
        assert_eq!(
            heard.captions,
            vec![
                (CaptionKind::Source, 0, "thanks my friend and the sea".to_string()),
                (CaptionKind::Translation, 0, "grazie my amico e the mare".to_string()),
            ]
        );
        assert!(pipeline.stats().partials.load(Ordering::Relaxed) as usize >= sources.len());
    }

    #[test]
    fn partials_can_be_turned_off() {
        let engine = Engine::sim();
        let config = Config { realtime: false, partial_interval_ms: 0, ..Config::default() };
        let pipeline = Pipeline::new(Arc::new(Engine::sim()), config);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(10, "en", "fr", false, sink).unwrap();
        play(&stream, spoken(&engine, "thanks my friend and the sea", "en"), true);
        stream.close();

        let heard = collect(frames);
        assert!(heard.partials.is_empty());
        assert_eq!(heard.captions.len(), 2);
        assert_eq!(pipeline.stats().partials.load(Ordering::Relaxed), 0);
    }

    #[test]
    fn utterances_stay_in_order() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(8192);
        let stream = pipeline.open(2, "it", "es", false, sink).unwrap();

        let phrases = ["ciao amico", "grazie", "acqua e casa", "notte"];
        let mut sequence = 0u16;
        for phrase in phrases {
            for packet in spoken(&engine, phrase, "it") {
                while !stream.push(sequence, packet.clone()) {
                    thread::sleep(Duration::from_millis(5));
                }
                sequence = sequence.wrapping_add(1);
            }
        }
        stream.close();

        let heard = collect(frames);
        let translations: Vec<&str> = heard
            .captions
            .iter()
            .filter(|(kind, ..)| *kind == CaptionKind::Translation)
            .map(|(_, _, text)| text.as_str())
            .collect();
        assert_eq!(translations, ["hola amigo", "gracias", "agua y casa", "noche"]);
        let indices: Vec<u32> = heard.captions.iter().map(|(_, index, _)| *index).collect();
        assert_eq!(indices, [0, 0, 1, 1, 2, 2, 3, 3]);
    }

    #[test]
    fn packet_loss_is_tolerated() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(3, "en", "fr", false, sink).unwrap();

        let packets = spoken(&engine, "hello friend", "en");
        let silent_prefix = 10;
        for (i, packet) in packets.into_iter().enumerate() {
            if i == silent_prefix / 2 || i == 1 {
                continue;
            }
            stream.push(i as u16, packet.clone());
            if i == 3 {
                stream.push(1, packet);
            }
        }
        stream.close();

        let heard = collect(frames);
        assert!(heard.captions.iter().any(|(_, _, text)| text == "bonjour ami"), "{:?}", heard.captions);
        assert_eq!(pipeline.stats().packets_lost.load(Ordering::Relaxed), 2);
        assert_eq!(pipeline.stats().packets_dropped.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn headphones_streams_start_speaking_before_the_sentence_ends() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(11, "en", "fr", true, sink).unwrap();
        let sentence = "hello world, and friend, water and house, night and sea and voice";
        let talker = {
            let packets = spoken(&engine, sentence, "en");
            thread::spawn(move || {
                play(&stream, packets, true);
                stream.close();
            })
        };

        let (mut packets, mut early, mut source) = (Vec::new(), 0, None);
        while let Ok(frame) = frames.recv_timeout(Duration::from_secs(20)) {
            match frame {
                Frame::Speech { packet, .. } => {
                    early += usize::from(source.is_none());
                    packets.push(packet);
                }
                Frame::Caption { kind: CaptionKind::Source, text, .. } => source = Some(text),
                Frame::Closed { .. } => break,
                _ => {}
            }
        }
        talker.join().unwrap();

        assert_eq!(source.as_deref(), Some(sentence));
        assert!(early > 50, "only {early} frames before the sentence ended");
        assert!(pipeline.stats().early_chunks.load(Ordering::Relaxed) >= 2);
        let words = |text: &str| text.replace(',', " ").split_whitespace().map(str::to_string).collect::<Vec<_>>();
        let expected = engine.translate(sentence, "en", "fr").unwrap();
        assert_eq!(words(&understand(&engine, &packets, "fr")), words(&expected));
    }

    #[test]
    fn long_translations_are_spoken_clause_by_clause() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(12, "en", "fr", false, sink).unwrap();
        let sentence = "thanks my friend, hello and water. the sea and house";
        play(&stream, spoken(&engine, sentence, "en"), false);
        stream.close();

        let heard = collect(frames);
        assert!(heard.errors.is_empty(), "{:?}", heard.errors);
        let expected = engine.translate(sentence, "en", "fr").unwrap();
        assert_eq!(heard.captions[1], (CaptionKind::Translation, 0, expected.clone()));
        assert_eq!(pipeline.stats().streamed_clauses.load(Ordering::Relaxed), 2);
        let words =
            |text: &str| text.replace([',', '.'], " ").split_whitespace().map(str::to_string).collect::<Vec<_>>();
        assert_eq!(words(&understand(&engine, &heard.packets, "fr")), words(&expected));
    }

    #[test]
    fn clauses_end_after_a_few_words_and_a_mark() {
        assert_eq!(clause_end("Hello, how are you?"), None);
        assert_eq!(clause_end("Hello, how are you? Fine"), Some(19));
        assert_eq!(clause_end("I am late, sorry"), Some(10));
        assert_eq!(clause_end("I am late,"), None);
        assert_eq!(clause_end("Ça a l'air super. Je"), Some(18));
        assert_eq!(clause_end("at 6:00 in the morning"), None);
    }

    #[test]
    fn early_chunks_follow_settled_words() {
        let mut early = Early::default();
        assert_eq!(early.advance("hello world, and", 8), None);
        assert_eq!(early.advance("hello world, and friend", 8), None);
        assert_eq!(early.advance("hello world, and friend water", 8), None);
        assert_eq!(early.advance("Hello world and friend, water and", 8), Some("Hello world and friend,".to_string()));
        assert_eq!(early.remainder("hello world and friend, water and house"), "water and house");
        assert_eq!(early.advance("one two three", 2), None);
        assert_eq!(early.advance("one two three four", 2), None);

        let mut early = Early::default();
        early.advance("a b c d e", 3);
        assert_eq!(early.advance("a b c d e f", 3), Some("a b c d e".to_string()));
        assert_eq!(early.advance("a b x", 3), None);
        assert_eq!(early.remainder("a b x d e f g"), "f g");
    }

    #[test]
    fn a_delivery_stall_does_not_break_the_utterance() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(8, "en", "fr", false, sink).unwrap();

        let packets = spoken(&engine, "hello world", "en");
        let middle = packets.len() / 2 - 10;
        for (i, packet) in packets.into_iter().enumerate() {
            if i == middle {
                thread::sleep(Duration::from_millis(220));
            }
            stream.push(i as u16, packet);
        }
        stream.close();

        let heard = collect(frames);
        assert!(heard.captions.iter().any(|(_, _, text)| text == "bonjour monde"), "{:?}", heard.captions);
    }

    #[test]
    fn garbage_packets_do_not_kill_the_stream() {
        let engine = Engine::sim();
        let pipeline = pipeline(false);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(4, "en", "it", false, sink).unwrap();

        let mut sequence = 0u16;
        for _ in 0..5 {
            stream.push(sequence, vec![0xff; 9]);
            sequence += 1;
        }
        for packet in spoken(&engine, "yes", "en") {
            stream.push(sequence, packet);
            sequence += 1;
        }
        stream.close();
        let heard = collect(frames);
        assert!(heard.captions.iter().any(|(_, _, text)| text == "sì"), "{:?}", heard.captions);
    }

    #[test]
    fn unsupported_languages_are_refused() {
        let pipeline = pipeline(false);
        let (sink, _frames) = mpsc::sync_channel(16);
        assert!(
            matches!(pipeline.open(5, "en", "de", false, sink), Err(OpenError::UnsupportedLanguage(l)) if l == "de")
        );
        assert_eq!(pipeline.stats().streams.load(Ordering::Relaxed), 0);
    }

    #[test]
    fn realtime_output_is_paced() {
        let engine = Engine::sim();
        let pipeline = pipeline(true);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(6, "en", "fr", false, sink).unwrap();
        for (i, packet) in spoken(&engine, "yes", "en").into_iter().enumerate() {
            stream.push(i as u16, packet);
        }

        let mut first = None;
        let mut count = 0u32;
        let mut last = Instant::now();
        let closer = thread::spawn(move || stream.close());
        while let Ok(frame) = frames.recv_timeout(Duration::from_secs(20)) {
            match frame {
                Frame::Speech { .. } => {
                    first.get_or_insert_with(Instant::now);
                    last = Instant::now();
                    count += 1;
                }
                Frame::Closed { .. } => break,
                _ => {}
            }
        }
        closer.join().unwrap();
        let elapsed = last.duration_since(first.unwrap());
        let nominal = Duration::from_millis(FRAME_MS * (count as u64 - 1));
        assert!(count > 20, "only {count} frames");
        assert!(elapsed + Duration::from_millis(60) >= nominal, "{elapsed:?} for {count} frames");
        assert!(elapsed <= nominal + Duration::from_millis(1500), "{elapsed:?} for {count} frames");
    }

    #[test]
    fn dropping_a_stream_stops_it_quickly() {
        let engine = Engine::sim();
        let pipeline = pipeline(true);
        let (sink, frames) = mpsc::sync_channel(4096);
        let stream = pipeline.open(7, "en", "fr", false, sink).unwrap();
        for (i, packet) in spoken(&engine, "hello world and friend", "en").into_iter().enumerate() {
            stream.push(i as u16, packet);
        }
        thread::sleep(Duration::from_millis(300));
        drop(stream);
        thread::sleep(Duration::from_millis(300));
        while frames.try_recv().is_ok() {}
        thread::sleep(Duration::from_millis(200));
        assert!(frames.try_recv().is_err());
        assert_eq!(pipeline.stats().streams.load(Ordering::Relaxed), 0);
    }

    #[test]
    fn stats_serialise_as_json() {
        let stats = Stats::default();
        stats.utterances.store(3, Ordering::Relaxed);
        let json = stats.to_json();
        assert!(json.starts_with('{') && json.ends_with('}'));
        assert!(json.contains("\"utterances\":3"));
        assert!(json.contains("\"streams\":0"));
    }

    #[test]
    fn packet_file_helpers_agree_with_the_pipeline() {
        let engine = Engine::sim();
        let packets = spoken(&engine, "voice", "en");
        let mut file = Vec::new();
        codec::write_packets(&mut file, &packets).unwrap();
        let restored = codec::read_packets(&mut file.as_slice()).unwrap();
        assert_eq!(understand(&engine, &restored, "en"), "voice");
    }
}
