use std::collections::VecDeque;

use crate::kernels;

#[derive(Clone, Debug)]
pub struct SegmenterConfig {
    pub sample_rate: u32,
    pub frame_ms: u32,
    pub threshold_db: f32,
    pub min_level_db: f32,
    pub attack_ms: u32,
    pub hangover_ms: u32,
    pub pre_roll_ms: u32,
    pub min_utterance_ms: u32,
    pub min_pitched_ms: u32,
    pub max_utterance_ms: u32,
}

impl Default for SegmenterConfig {
    fn default() -> Self {
        Self {
            sample_rate: 16000,
            frame_ms: 20,
            threshold_db: 9.0,
            min_level_db: -50.0,
            attack_ms: 40,
            hangover_ms: 600,
            pre_roll_ms: 300,
            min_utterance_ms: 200,
            min_pitched_ms: 60,
            max_utterance_ms: 15_000,
        }
    }
}

const FLOOR_MIN_DB: f32 = -75.0;
const FLOOR_MAX_DB: f32 = -30.0;
const FLOOR_FALL: f32 = 0.2;
const FLOOR_RISE: f32 = 0.02;
const FLOOR_CREEP_DB: f32 = 0.02;
const TRAILING_MS: u32 = 200;
const PITCH_MIN_HZ: u32 = 70;
const PITCH_MAX_HZ: u32 = 400;
const PITCH_CORRELATION: f32 = 0.6;
const PITCH_DIP: f32 = 0.4;

// A voice is periodic: the vocal folds repeat every 2.5 to 14 ms. A knock, a
// door or a cough is not, and whisper turns such sounds into "thank you".
// The frame is pitched when its autocorrelation peaks at such a lag; the peak
// must be a real one, not the slow decay of a low rumble.
fn pitched(window: &[f32], sample_rate: u32, squares: &mut Vec<f32>) -> bool {
    let (min_lag, max_lag) = ((sample_rate / PITCH_MAX_HZ) as usize, (sample_rate / PITCH_MIN_HZ) as usize);
    if window.len() <= max_lag + 1 || min_lag < 3 {
        return false;
    }
    squares.clear();
    squares.push(0.0);
    let mut total = 0.0;
    for x in window {
        total += x * x;
        squares.push(total);
    }
    let correlation = |lag: usize| {
        let n = window.len() - lag;
        let energy = squares[n] + squares[window.len()] - squares[lag];
        if energy <= 1e-9 { 0.0 } else { 2.0 * kernels::dot(&window[..n], &window[lag..]) / energy }
    };
    let (mut lowest, mut before, mut here) = (f32::MAX, correlation(1), correlation(2));
    for lag in 2..=max_lag {
        let after = correlation(lag + 1);
        let peak = here >= PITCH_CORRELATION && here > before && here >= after;
        if lag >= min_lag && peak && lowest <= here - PITCH_DIP {
            return true;
        }
        lowest = lowest.min(here);
        (before, here) = (here, after);
    }
    false
}

pub struct Segmenter {
    config: SegmenterConfig,
    frame_len: usize,
    pending: Vec<f32>,
    pre_roll: VecDeque<f32>,
    utterance: Vec<f32>,
    floor_db: f32,
    speech_frames: u32,
    silence_frames: u32,
    voiced_frames: u32,
    pitched_frames: u32,
    window: Vec<f32>,
    squares: Vec<f32>,
    active: bool,
}

impl Segmenter {
    pub fn new(config: SegmenterConfig) -> Self {
        let frame_len = (config.sample_rate * config.frame_ms / 1000) as usize;
        assert!(frame_len > 0);
        Self {
            config,
            frame_len,
            pending: Vec::new(),
            pre_roll: VecDeque::new(),
            utterance: Vec::new(),
            floor_db: -60.0,
            speech_frames: 0,
            silence_frames: 0,
            voiced_frames: 0,
            pitched_frames: 0,
            window: Vec::new(),
            squares: Vec::new(),
            active: false,
        }
    }

    pub fn is_active(&self) -> bool {
        self.active
    }

    pub fn open_utterance(&self) -> Option<&[f32]> {
        let heard = self.pitched_frames >= self.frames(self.config.min_pitched_ms);
        (self.active && heard).then_some(self.utterance.as_slice())
    }

    pub fn noise_floor_db(&self) -> f32 {
        self.floor_db
    }

    pub fn push(&mut self, samples: &[f32], mut emit: impl FnMut(Vec<f32>)) {
        self.pending.extend_from_slice(samples);
        let mut offset = 0;
        while offset + self.frame_len <= self.pending.len() {
            let frame: Vec<f32> = self.pending[offset..offset + self.frame_len].to_vec();
            offset += self.frame_len;
            self.push_frame(&frame, &mut emit);
        }
        self.pending.drain(..offset);
    }

    pub fn flush(&mut self, mut emit: impl FnMut(Vec<f32>)) {
        self.pending.clear();
        if self.active {
            self.finish(&mut emit);
        }
        self.pre_roll.clear();
        self.speech_frames = 0;
    }

    fn frames(&self, ms: u32) -> u32 {
        ms.div_ceil(self.config.frame_ms).max(1)
    }

    fn samples(&self, ms: u32) -> usize {
        (self.config.sample_rate as u64 * ms as u64 / 1000) as usize
    }

    fn push_frame(&mut self, frame: &[f32], emit: &mut impl FnMut(Vec<f32>)) {
        let level_db = 10.0 * (kernels::energy(frame) / frame.len() as f32 + 1e-12).log10();
        let speech = level_db > self.floor_db + self.config.threshold_db && level_db > self.config.min_level_db;
        self.track_floor(level_db, speech);
        let keep = self.window.len().saturating_sub(self.frame_len);
        self.window.drain(..keep);
        self.window.extend_from_slice(frame);
        let voice = speech && pitched(&self.window, self.config.sample_rate, &mut self.squares);

        if !self.active {
            self.pre_roll.extend(frame);
            let keep = self.samples(self.config.pre_roll_ms);
            while self.pre_roll.len() > keep {
                self.pre_roll.pop_front();
            }
            self.speech_frames = if speech { self.speech_frames + 1 } else { 0 };
            if self.speech_frames >= self.frames(self.config.attack_ms) {
                self.active = true;
                self.voiced_frames = self.speech_frames;
                self.pitched_frames = u32::from(voice);
                self.silence_frames = 0;
                self.utterance.extend(self.pre_roll.drain(..));
            }
            return;
        }

        self.utterance.extend_from_slice(frame);
        if speech {
            self.silence_frames = 0;
            self.voiced_frames += 1;
            self.pitched_frames += u32::from(voice);
        } else {
            self.silence_frames += 1;
        }

        let too_long = self.utterance.len() >= self.samples(self.config.max_utterance_ms);
        if too_long || self.silence_frames >= self.frames(self.config.hangover_ms) {
            self.finish(emit);
        }
    }

    fn track_floor(&mut self, level_db: f32, speech: bool) {
        if speech {
            self.floor_db += FLOOR_CREEP_DB;
        } else if level_db < self.floor_db {
            self.floor_db += FLOOR_FALL * (level_db - self.floor_db);
        } else {
            self.floor_db += FLOOR_RISE * (level_db - self.floor_db);
        }
        self.floor_db = self.floor_db.clamp(FLOOR_MIN_DB, FLOOR_MAX_DB);
    }

    fn finish(&mut self, emit: &mut impl FnMut(Vec<f32>)) {
        let silence = self.silence_frames as usize * self.frame_len;
        let trim = silence.saturating_sub(self.samples(TRAILING_MS));
        let mut utterance = std::mem::take(&mut self.utterance);
        utterance.truncate(utterance.len() - trim.min(utterance.len()));

        let voiced_ms = self.voiced_frames * self.config.frame_ms;
        let pitched_ms = self.pitched_frames * self.config.frame_ms;
        self.active = false;
        self.speech_frames = 0;
        self.silence_frames = 0;
        self.voiced_frames = 0;
        self.pitched_frames = 0;
        if voiced_ms >= self.config.min_utterance_ms && pitched_ms >= self.config.min_pitched_ms {
            emit(utterance);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const RATE: u32 = 16000;

    fn tone(seconds: f64, amplitude: f32) -> Vec<f32> {
        (0..(RATE as f64 * seconds) as usize)
            .map(|i| amplitude * (2.0 * std::f32::consts::PI * 440.0 * i as f32 / RATE as f32).sin())
            .collect()
    }

    fn noise(seconds: f64, amplitude: f32, seed: u64) -> Vec<f32> {
        let mut state = seed;
        (0..(RATE as f64 * seconds) as usize)
            .map(|_| {
                state = state.wrapping_mul(6364136223846793005).wrapping_add(1442695040888963407);
                amplitude * (((state >> 40) as f32 / (1u64 << 24) as f32) * 2.0 - 1.0)
            })
            .collect()
    }

    fn run(config: SegmenterConfig, audio: &[f32]) -> Vec<Vec<f32>> {
        let mut segmenter = Segmenter::new(config);
        let mut out = Vec::new();
        for chunk in audio.chunks(313) {
            segmenter.push(chunk, |u| out.push(u));
        }
        segmenter.flush(|u| out.push(u));
        out
    }

    fn seconds(samples: &[f32]) -> f64 {
        samples.len() as f64 / RATE as f64
    }

    #[test]
    fn silence_produces_nothing() {
        assert!(run(SegmenterConfig::default(), &vec![0.0; RATE as usize * 3]).is_empty());
        assert!(run(SegmenterConfig::default(), &noise(3.0, 0.0005, 1)).is_empty());
    }

    #[test]
    fn one_burst_becomes_one_utterance() {
        let mut audio = noise(1.0, 0.0005, 2);
        audio.extend(tone(1.0, 0.3));
        audio.extend(noise(1.5, 0.0005, 3));
        let utterances = run(SegmenterConfig::default(), &audio);
        assert_eq!(utterances.len(), 1);
        let length = seconds(&utterances[0]);
        assert!((1.2..=1.6).contains(&length), "utterance lasted {length} s");
    }

    #[test]
    fn pre_roll_keeps_the_onset() {
        let mut audio = vec![0.0; RATE as usize];
        audio.extend(tone(0.5, 0.3));
        audio.extend(vec![0.0; RATE as usize]);
        let utterances = run(SegmenterConfig::default(), &audio);
        assert_eq!(utterances.len(), 1);
        let first_loud = utterances[0].iter().position(|s| s.abs() > 0.1).unwrap();
        assert!(first_loud >= RATE as usize / 5, "onset at sample {first_loud}");
    }

    #[test]
    fn short_pauses_do_not_split() {
        let mut audio = vec![0.0; 8000];
        audio.extend(tone(0.5, 0.3));
        audio.extend(vec![0.0; 4800]);
        audio.extend(tone(0.5, 0.3));
        audio.extend(vec![0.0; 16000]);
        assert_eq!(run(SegmenterConfig::default(), &audio).len(), 1);
    }

    #[test]
    fn long_pauses_split() {
        let mut audio = vec![0.0; 8000];
        audio.extend(tone(0.5, 0.3));
        audio.extend(vec![0.0; 16000]);
        audio.extend(tone(0.5, 0.3));
        audio.extend(vec![0.0; 16000]);
        assert_eq!(run(SegmenterConfig::default(), &audio).len(), 2);
    }

    #[test]
    fn clicks_are_ignored() {
        let mut audio = vec![0.0; 16000];
        audio.extend(tone(0.06, 0.5));
        audio.extend(vec![0.0; 32000]);
        assert!(run(SegmenterConfig::default(), &audio).is_empty());
    }

    fn knock(seconds: f64, amplitude: f32, smoothing: f32, seed: u64) -> Vec<f32> {
        let mut low = 0.0;
        let raw = noise(seconds, 1.0, seed);
        let len = raw.len() as f32;
        raw.iter()
            .enumerate()
            .map(|(i, x)| {
                low += (1.0 - smoothing) * (x - low);
                amplitude * low * (-4.0 * i as f32 / len).exp()
            })
            .collect()
    }

    #[test]
    fn knocks_and_rumbles_are_not_speech() {
        for (smoothing, seed) in [0.0f32, 0.5, 0.9, 0.95, 0.98, 0.99, 0.995, 0.998]
            .into_iter()
            .flat_map(|s| (0..5).map(move |k| (s, 100 + k)))
        {
            let mut segmenter = Segmenter::new(SegmenterConfig::default());
            let mut audio = vec![0.0; 8000];
            let gain = 0.5 / (1.0 - smoothing).sqrt();
            audio.extend(knock(0.6, gain, smoothing, seed));
            let mut out = Vec::new();
            segmenter.push(&audio, |u| out.push(u));
            assert!(segmenter.open_utterance().is_none(), "smoothing {smoothing}: partial offered");
            segmenter.push(&vec![0.0; 24000], |u| out.push(u));
            assert!(out.is_empty(), "smoothing {smoothing}: {} utterances", out.len());
        }
    }

    #[test]
    fn low_and_high_voices_are_speech() {
        for hz in [85.0, 120.0, 220.0, 380.0] {
            let mut audio = vec![0.0; 8000];
            audio.extend((0..8000).map(|i| {
                let t = i as f32 / RATE as f32;
                (1..=6).map(|k| 0.3 / k as f32 * (2.0 * std::f32::consts::PI * hz * k as f32 * t).sin()).sum::<f32>()
            }));
            audio.extend(vec![0.0; 16000]);
            assert_eq!(run(SegmenterConfig::default(), &audio).len(), 1, "{hz} Hz");
        }
    }

    #[test]
    fn long_speech_is_cut_at_the_limit() {
        let config = SegmenterConfig { max_utterance_ms: 2000, ..SegmenterConfig::default() };
        let mut audio = vec![0.0; 8000];
        audio.extend(tone(5.0, 0.3));
        audio.extend(vec![0.0; 16000]);
        let utterances = run(config, &audio);
        assert!(utterances.len() >= 2, "got {} utterances", utterances.len());
        assert!(utterances.iter().all(|u| seconds(u) <= 2.05));
    }

    #[test]
    fn the_open_utterance_grows_until_it_is_emitted() {
        let mut segmenter = Segmenter::new(SegmenterConfig::default());
        segmenter.push(&vec![0.0; 8000], |_| panic!("silence emitted"));
        assert!(segmenter.open_utterance().is_none());

        let speech = tone(1.0, 0.3);
        segmenter.push(&speech[..8000], |_| panic!("emitted too early"));
        let first = segmenter.open_utterance().unwrap().len();
        segmenter.push(&speech[8000..], |_| panic!("emitted too early"));
        let second = segmenter.open_utterance().unwrap().len();
        assert!(first > 4000 && second >= first + 8000, "{first} then {second}");

        let mut emitted = Vec::new();
        segmenter.push(&vec![0.0; 16000], |u| emitted.push(u));
        assert_eq!(emitted.len(), 1);
        assert!(segmenter.open_utterance().is_none());
    }

    #[test]
    fn flush_emits_the_open_utterance() {
        let mut audio = vec![0.0; 8000];
        audio.extend(tone(1.0, 0.3));
        let utterances = run(SegmenterConfig::default(), &audio);
        assert_eq!(utterances.len(), 1);
    }

    #[test]
    fn speech_over_steady_noise_is_detected() {
        let mut audio = noise(2.0, 0.01, 4);
        let mut burst = noise(1.0, 0.01, 5);
        for (b, t) in burst.iter_mut().zip(tone(1.0, 0.3)) {
            *b += t;
        }
        audio.extend(burst);
        audio.extend(noise(2.0, 0.01, 6));
        assert_eq!(run(SegmenterConfig::default(), &audio).len(), 1);
    }
}
