use std::collections::HashMap;
use std::fmt::Write as _;
use std::fs::File;
use std::io::{self, BufReader};
use std::path::Path;
use std::time::Instant;

use ambivox_dsp::resample::resample;
use ambivox_engine::{ASR_SAMPLE_RATE, Engine};

use crate::pipeline::clause_end;
use crate::wav;

const CHRF_ORDER: usize = 6;
const CHRF_BETA: f64 = 2.0;
const LEAD_MS: usize = 300;
const TAIL_MS: usize = 600;
const FAR_CHRF: f64 = 50.0;

#[derive(Clone, Debug, PartialEq)]
pub struct Case {
    pub id: String,
    pub source: String,
    pub target: String,
    pub text: String,
    pub reference: String,
}

pub fn parse_set(text: &str) -> io::Result<Vec<Case>> {
    let mut cases = Vec::new();
    for (number, line) in text.lines().enumerate() {
        let line = line.trim_end_matches('\r');
        if line.trim().is_empty() || line.starts_with('#') {
            continue;
        }
        let fields: Vec<&str> = line.split('|').map(str::trim).collect();
        let [id, source, target, text, reference] = fields[..] else {
            let message = format!("line {}: expected 5 fields separated by |, found {}", number + 1, fields.len());
            return Err(io::Error::new(io::ErrorKind::InvalidData, message));
        };
        cases.push(Case {
            id: id.to_string(),
            source: source.to_string(),
            target: target.to_string(),
            text: text.to_string(),
            reference: reference.to_string(),
        });
    }
    Ok(cases)
}

const NUMBERS: [(&str, [&str; 21]); 4] = [
    (
        "en",
        [
            "zero",
            "one",
            "two",
            "three",
            "four",
            "five",
            "six",
            "seven",
            "eight",
            "nine",
            "ten",
            "eleven",
            "twelve",
            "thirteen",
            "fourteen",
            "fifteen",
            "sixteen",
            "seventeen",
            "eighteen",
            "nineteen",
            "twenty",
        ],
    ),
    (
        "fr",
        [
            "zéro", "un", "deux", "trois", "quatre", "cinq", "six", "sept", "huit", "neuf", "dix", "onze", "douze",
            "treize", "quatorze", "quinze", "seize", "dix-sept", "dix-huit", "dix-neuf", "vingt",
        ],
    ),
    (
        "es",
        [
            "cero",
            "uno",
            "dos",
            "tres",
            "cuatro",
            "cinco",
            "seis",
            "siete",
            "ocho",
            "nueve",
            "diez",
            "once",
            "doce",
            "trece",
            "catorce",
            "quince",
            "dieciséis",
            "diecisiete",
            "dieciocho",
            "diecinueve",
            "veinte",
        ],
    ),
    (
        "it",
        [
            "zero",
            "uno",
            "due",
            "tre",
            "quattro",
            "cinque",
            "sei",
            "sette",
            "otto",
            "nove",
            "dieci",
            "undici",
            "dodici",
            "tredici",
            "quattordici",
            "quindici",
            "sedici",
            "diciassette",
            "diciotto",
            "diciannove",
            "venti",
        ],
    ),
];

const TENS: [(&str, [&str; 4]); 4] = [
    ("en", ["thirty", "forty", "fifty", "sixty"]),
    ("fr", ["trente", "quarante", "cinquante", "soixante"]),
    ("es", ["treinta", "cuarenta", "cincuenta", "sesenta"]),
    ("it", ["trenta", "quaranta", "cinquanta", "sessanta"]),
];

const CONTRACTED: [&str; 14] =
    ["it", "that", "what", "where", "there", "here", "he", "she", "who", "how", "when", "why", "this", "everything"];

fn spoken_number(lang: &str, word: &str) -> Option<u32> {
    let units = NUMBERS.iter().find(|(l, _)| *l == lang).map(|(_, w)| w)?;
    if let Some(n) = units.iter().position(|w| *w == word) {
        return Some(n as u32);
    }
    let tens = TENS.iter().find(|(l, _)| *l == lang).map(|(_, w)| w)?;
    tens.iter().position(|w| *w == word).map(|n| 30 + 10 * n as u32)
}

fn expand_english(word: &str, out: &mut Vec<String>) {
    let split = |stem: &str, tail: &str, out: &mut Vec<String>| {
        out.push(stem.to_string());
        out.push(tail.to_string());
    };
    match word {
        "can't" | "cannot" => split("can", "not", out),
        "won't" => split("will", "not", out),
        "let's" => split("let", "us", out),
        _ => match word.split_once('\'') {
            Some((stem, "t")) if stem.ends_with('n') => split(&stem[..stem.len() - 1], "not", out),
            Some((stem, "re")) => split(stem, "are", out),
            Some((stem, "ve")) => split(stem, "have", out),
            Some((stem, "ll")) => split(stem, "will", out),
            Some((stem, "m")) => split(stem, "am", out),
            Some((stem, "d")) => split(stem, "would", out),
            Some((stem, "s")) if CONTRACTED.contains(&stem) => split(stem, "is", out),
            _ => out.push(word.to_string()),
        },
    }
}

pub fn words(text: &str, lang: &str) -> Vec<String> {
    let lowered: String =
        text.chars().flat_map(char::to_lowercase).map(|c| if c == '\u{2019}' { '\'' } else { c }).collect();
    let mut expanded = Vec::new();
    for word in lowered.split_whitespace() {
        let word = word.trim_matches(|c: char| !c.is_alphanumeric() && c != '\'');
        if lang == "en" {
            expand_english(word, &mut expanded);
        } else {
            expanded.push(word.to_string());
        }
    }
    let mut out = Vec::new();
    for word in expanded {
        let compound = spoken_number(lang, &word).is_some();
        let mut piece = String::new();
        for c in word.chars() {
            let kind_changes = piece.chars().last().is_some_and(|last| last.is_ascii_digit() != c.is_ascii_digit());
            if (!c.is_alphanumeric() && !compound) || kind_changes {
                push_token(lang, std::mem::take(&mut piece), &mut out);
            }
            if c.is_alphanumeric() || compound {
                piece.push(c);
            }
        }
        push_token(lang, piece, &mut out);
    }
    out
}

fn push_token(lang: &str, token: String, out: &mut Vec<String>) {
    if token.is_empty() {
        return;
    }
    match token.parse::<u32>().ok().or_else(|| spoken_number(lang, &token)) {
        Some(n) if n > 20 && n < 70 && n % 10 != 0 => {
            out.push((n - n % 10).to_string());
            out.push((n % 10).to_string());
        }
        Some(n) => out.push(n.to_string()),
        None if lang == "fr" && token == "h" => out.push("heures".to_string()),
        None => out.push(token),
    }
}

pub fn word_errors(reference: &str, hypothesis: &str, lang: &str) -> (usize, usize) {
    let (reference, hypothesis) = (words(reference, lang), words(hypothesis, lang));
    let mut row: Vec<usize> = (0..=hypothesis.len()).collect();
    for (i, expected) in reference.iter().enumerate() {
        let mut diagonal = row[0];
        row[0] = i + 1;
        for (j, heard) in hypothesis.iter().enumerate() {
            let substitution = diagonal + usize::from(expected != heard);
            diagonal = row[j + 1];
            row[j + 1] = substitution.min(row[j] + 1).min(row[j + 1] + 1);
        }
    }
    (row[hypothesis.len()], reference.len())
}

fn grams(chars: &[char], n: usize) -> HashMap<&[char], usize> {
    let mut counts = HashMap::new();
    for gram in chars.windows(n) {
        *counts.entry(gram).or_default() += 1;
    }
    counts
}

#[derive(Clone, Copy, Debug, Default)]
pub struct Chrf {
    matches: [usize; CHRF_ORDER],
    hypothesis: [usize; CHRF_ORDER],
    reference: [usize; CHRF_ORDER],
}

impl Chrf {
    pub fn add(&mut self, reference: &str, hypothesis: &str) {
        let squeeze = |text: &str| -> Vec<char> {
            text.chars().filter(|c| !c.is_whitespace()).flat_map(char::to_lowercase).collect()
        };
        let (reference, hypothesis) = (squeeze(reference), squeeze(hypothesis));
        for n in 1..=CHRF_ORDER {
            let (expected, produced) = (grams(&reference, n), grams(&hypothesis, n));
            let matched: usize =
                produced.iter().map(|(gram, &count)| count.min(expected.get(gram).copied().unwrap_or(0))).sum();
            self.matches[n - 1] += matched;
            self.hypothesis[n - 1] += hypothesis.len().saturating_sub(n - 1);
            self.reference[n - 1] += reference.len().saturating_sub(n - 1);
        }
    }

    pub fn merge(&mut self, other: &Chrf) {
        for n in 0..CHRF_ORDER {
            self.matches[n] += other.matches[n];
            self.hypothesis[n] += other.hypothesis[n];
            self.reference[n] += other.reference[n];
        }
    }

    pub fn score(&self) -> f64 {
        let (mut precision, mut recall, mut orders) = (0.0, 0.0, 0);
        for n in 0..CHRF_ORDER {
            if self.hypothesis[n] > 0 {
                precision += self.matches[n] as f64 / self.hypothesis[n] as f64;
            }
            if self.reference[n] > 0 {
                recall += self.matches[n] as f64 / self.reference[n] as f64;
            }
            orders += usize::from(self.hypothesis[n] > 0 && self.reference[n] > 0);
        }
        if orders == 0 || precision + recall == 0.0 {
            return 0.0;
        }
        let (precision, recall) = (precision / orders as f64, recall / orders as f64);
        let beta2 = CHRF_BETA * CHRF_BETA;
        100.0 * (1.0 + beta2) * precision * recall / (beta2 * precision + recall)
    }
}

#[derive(Clone, Debug)]
pub struct Outcome {
    pub case: Case,
    pub recorded: bool,
    pub heard: String,
    pub translation: String,
    pub word_errors: (usize, usize),
    pub listen_ms: u64,
    pub translate_ms: u64,
    pub speak_ms: u64,
    pub voice_ms: u64,
    chrf: Chrf,
    chrf_text: Chrf,
}

impl Outcome {
    pub fn total_ms(&self) -> u64 {
        self.listen_ms + self.translate_ms + self.speak_ms
    }
}

fn milliseconds(started: Instant) -> u64 {
    started.elapsed().as_millis() as u64
}

fn padded(samples: &[f32]) -> Vec<f32> {
    let at = |ms: usize| ASR_SAMPLE_RATE as usize * ms / 1000;
    let mut pcm = vec![0.0; at(LEAD_MS)];
    pcm.extend_from_slice(samples);
    pcm.resize(pcm.len() + at(TAIL_MS), 0.0);
    pcm
}

fn speech(
    engine: &Engine,
    case: &Case,
    recordings: Option<&Path>,
) -> Result<(Vec<f32>, bool), Box<dyn std::error::Error>> {
    if let Some(path) = recordings.map(|dir| dir.join(format!("{}.wav", case.id))).filter(|path| path.exists()) {
        let (samples, rate) =
            wav::read(&mut BufReader::new(File::open(&path)?)).map_err(|e| format!("{}: {e}", path.display()))?;
        return Ok((padded(&resample(&samples, rate, ASR_SAMPLE_RATE)), true));
    }
    let audio = engine.synthesize(&case.text, &case.source)?;
    Ok((padded(&resample(&audio.samples, audio.sample_rate, ASR_SAMPLE_RATE)), false))
}

fn warm_up_pair(engine: &Engine, case: &Case) -> Result<(), ambivox_engine::Error> {
    let audio = engine.synthesize(&case.text, &case.source)?;
    engine.transcribe(&padded(&resample(&audio.samples, audio.sample_rate, ASR_SAMPLE_RATE)), &case.source)?;
    let translated = engine.translate(&case.text, &case.source, &case.target)?;
    engine.synthesize(if translated.trim().is_empty() { &case.reference } else { &translated }, &case.target)?;
    Ok(())
}

pub fn warm_up(engine: &Engine, cases: &mut Vec<Case>) -> Vec<String> {
    let mut seen: Vec<(String, String)> = Vec::new();
    let mut skipped = Vec::new();
    let mut failed: Vec<(String, String)> = Vec::new();
    for case in cases.iter() {
        let pair = (case.source.clone(), case.target.clone());
        if seen.contains(&pair) {
            continue;
        }
        if let Err(error) = warm_up_pair(engine, case) {
            skipped.push(format!("{}-{}: {error}", case.source, case.target));
            failed.push(pair.clone());
        }
        seen.push(pair);
    }
    cases.retain(|case| !failed.iter().any(|(source, target)| &case.source == source && &case.target == target));
    skipped
}

pub fn measure(engine: &Engine, case: &Case, recordings: Option<&Path>) -> Result<Outcome, Box<dyn std::error::Error>> {
    let (pcm, recorded) = speech(engine, case, recordings)?;

    let started = Instant::now();
    let heard = engine.transcribe(&pcm, &case.source)?.trim().to_string();
    let listen_ms = milliseconds(started);

    let started = Instant::now();
    let mut first = None;
    let translation = if heard.is_empty() {
        String::new()
    } else {
        let follow = |text: &str| {
            if first.is_none() {
                first = clause_end(text).map(|end| (milliseconds(started), text[..end].trim().to_string()));
            }
        };
        engine.translate_with(&heard, &case.source, &case.target, follow)?.trim().to_string()
    };
    let translate_ms = milliseconds(started);

    let started = Instant::now();
    if !translation.is_empty() {
        engine.synthesize(&translation, &case.target)?;
    }
    let speak_ms = milliseconds(started);

    let voice_ms = listen_ms
        + match first {
            Some((at, clause)) => {
                let started = Instant::now();
                engine.synthesize(&clause, &case.target)?;
                at + milliseconds(started)
            }
            None => translate_ms + speak_ms,
        };

    let mut chrf = Chrf::default();
    chrf.add(&case.reference, &translation);
    let mut chrf_text = Chrf::default();
    chrf_text.add(&case.reference, engine.translate(&case.text, &case.source, &case.target)?.trim());

    Ok(Outcome {
        word_errors: word_errors(&case.text, &heard, &case.source),
        case: case.clone(),
        recorded,
        heard,
        translation,
        listen_ms,
        translate_ms,
        speak_ms,
        voice_ms,
        chrf,
        chrf_text,
    })
}

#[derive(Clone, Debug)]
pub struct Summary {
    pub label: String,
    pub count: usize,
    pub word_error_rate: f64,
    pub chrf: f64,
    pub chrf_text: f64,
    pub listen_ms: u64,
    pub translate_ms: u64,
    pub speak_ms: u64,
    pub voice_p50_ms: u64,
    pub total_p50_ms: u64,
    pub total_p90_ms: u64,
}

fn percentile(mut values: Vec<u64>, fraction: f64) -> u64 {
    if values.is_empty() {
        return 0;
    }
    values.sort_unstable();
    let rank = (fraction * values.len() as f64).ceil() as usize;
    values[rank.clamp(1, values.len()) - 1]
}

pub fn summarize(label: &str, outcomes: &[&Outcome]) -> Summary {
    let (mut errors, mut total_words) = (0, 0);
    let (mut chrf, mut chrf_text) = (Chrf::default(), Chrf::default());
    for outcome in outcomes {
        errors += outcome.word_errors.0;
        total_words += outcome.word_errors.1;
        chrf.merge(&outcome.chrf);
        chrf_text.merge(&outcome.chrf_text);
    }
    let median = |pick: fn(&Outcome) -> u64| percentile(outcomes.iter().map(|o| pick(o)).collect(), 0.5);
    Summary {
        label: label.to_string(),
        count: outcomes.len(),
        word_error_rate: if total_words == 0 { 0.0 } else { 100.0 * errors as f64 / total_words as f64 },
        chrf: chrf.score(),
        chrf_text: chrf_text.score(),
        listen_ms: median(|o| o.listen_ms),
        translate_ms: median(|o| o.translate_ms),
        speak_ms: median(|o| o.speak_ms),
        voice_p50_ms: median(|o| o.voice_ms),
        total_p50_ms: median(Outcome::total_ms),
        total_p90_ms: percentile(outcomes.iter().map(|o| o.total_ms()).collect(), 0.9),
    }
}

pub fn summaries(outcomes: &[Outcome]) -> Vec<Summary> {
    let mut pairs: Vec<(String, String)> = Vec::new();
    for outcome in outcomes {
        let pair = (outcome.case.source.clone(), outcome.case.target.clone());
        if !pairs.contains(&pair) {
            pairs.push(pair);
        }
    }
    let mut rows: Vec<Summary> = pairs
        .iter()
        .map(|(source, target)| {
            let group: Vec<&Outcome> =
                outcomes.iter().filter(|o| &o.case.source == source && &o.case.target == target).collect();
            summarize(&format!("{source}-{target}"), &group)
        })
        .collect();
    if rows.len() > 1 {
        rows.push(summarize("all", &outcomes.iter().collect::<Vec<_>>()));
    }
    rows
}

pub fn table(rows: &[Summary]) -> String {
    let mut out = String::new();
    let _ = writeln!(
        out,
        "{:<6} {:>4} {:>7} {:>6} {:>10} {:>8} {:>10} {:>8} {:>10} {:>10} {:>10}",
        "pair", "n", "WER", "chrF", "chrF text", "listen", "translate", "speak", "voice p50", "total p50", "total p90"
    );
    for row in rows {
        let _ = writeln!(
            out,
            "{:<6} {:>4} {:>6.1}% {:>6.1} {:>10.1} {:>5} ms {:>7} ms {:>5} ms {:>7} ms {:>7} ms {:>7} ms",
            row.label,
            row.count,
            row.word_error_rate,
            row.chrf,
            row.chrf_text,
            row.listen_ms,
            row.translate_ms,
            row.speak_ms,
            row.voice_p50_ms,
            row.total_p50_ms,
            row.total_p90_ms
        );
    }
    out
}

pub fn misheard(outcomes: &[Outcome]) -> String {
    let mut out = String::new();
    for outcome in outcomes.iter().filter(|o| o.word_errors.0 > 0) {
        let _ = writeln!(out, "{}  said:  {}", outcome.case.id, outcome.case.text);
        let _ = writeln!(out, "{}  heard: {}", " ".repeat(outcome.case.id.chars().count()), outcome.heard);
    }
    out
}

pub fn mistranslated(outcomes: &[Outcome]) -> String {
    let mut out = String::new();
    for outcome in outcomes.iter().filter(|o| !o.translation.is_empty() && o.chrf.score() < FAR_CHRF) {
        let pad = " ".repeat(outcome.case.id.chars().count());
        let _ = writeln!(out, "{}  heard:     {}", outcome.case.id, outcome.heard);
        let _ = writeln!(out, "{pad}  got:       {}", outcome.translation);
        let _ = writeln!(out, "{pad}  reference: {}", outcome.case.reference);
    }
    out
}

fn quoted(text: &str) -> String {
    let mut out = String::from("\"");
    for c in text.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

pub fn json(backend: &str, whisper: &str, translator: &str, rows: &[Summary], outcomes: &[Outcome]) -> String {
    let summaries: Vec<String> = rows
        .iter()
        .map(|row| {
            format!(
                "{{\"pair\":{},\"n\":{},\"wer\":{:.2},\"chrf\":{:.2},\"chrf_text\":{:.2},\"listen_ms\":{},\"translate_ms\":{},\"speak_ms\":{},\"voice_p50_ms\":{},\"total_p50_ms\":{},\"total_p90_ms\":{}}}",
                quoted(&row.label),
                row.count,
                row.word_error_rate,
                row.chrf,
                row.chrf_text,
                row.listen_ms,
                row.translate_ms,
                row.speak_ms,
                row.voice_p50_ms,
                row.total_p50_ms,
                row.total_p90_ms
            )
        })
        .collect();
    let cases: Vec<String> = outcomes
        .iter()
        .map(|o| {
            format!(
                "{{\"id\":{},\"recorded\":{},\"heard\":{},\"translation\":{},\"word_errors\":{},\"words\":{},\"chrf\":{:.2},\"voice_ms\":{},\"total_ms\":{}}}",
                quoted(&o.case.id),
                o.recorded,
                quoted(&o.heard),
                quoted(&o.translation),
                o.word_errors.0,
                o.word_errors.1,
                o.chrf.score(),
                o.voice_ms,
                o.total_ms()
            )
        })
        .collect();
    format!(
        "{{\"backend\":{},\"whisper\":{},\"translator\":{},\"summary\":[{}],\"cases\":[{}]}}\n",
        quoted(backend),
        quoted(whisper),
        quoted(translator),
        summaries.join(","),
        cases.join(",")
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sets_are_separated_by_bars() {
        let set = "# comment\n\nfr-en-01 | fr | en | Bonjour, ça va ? | Hello, how are you?\r\n";
        let cases = parse_set(set).unwrap();
        assert_eq!(cases.len(), 1);
        assert_eq!(cases[0].text, "Bonjour, ça va ?");
        assert_eq!(cases[0].reference, "Hello, how are you?");
        assert!(parse_set("fr-en-01 | fr | en | Bonjour.\n").is_err());
    }

    #[test]
    fn word_errors_ignore_case_and_punctuation() {
        assert_eq!(word_errors("Bonjour, comment ça va ?", "bonjour comment ça va", "fr"), (0, 4));
        assert_eq!(word_errors("C'est pas grave.", "c'est pas grave", "fr"), (0, 4));
        assert_eq!(word_errors("the cat sat", "the hat sat down", "en"), (2, 3));
        assert_eq!(word_errors("one two three", "", "en"), (3, 3));
        assert_eq!(word_errors("", "noise", "en"), (1, 0));
    }

    #[test]
    fn numbers_count_the_same_spelled_or_in_digits() {
        assert_eq!(word_errors("devant la gare à six heures", "devant la gare à 6 heures", "fr"), (0, 6));
        assert_eq!(word_errors("mardi à quatorze heures trente", "mardi à 14h30", "fr"), (0, 5));
        assert_eq!(word_errors("call you back in ten minutes", "call you back in 10 minutes", "en"), (0, 6));
        assert_eq!(word_errors("leaves at seven fifteen", "leaves at 7.15", "en"), (0, 4));
        assert_eq!(word_errors("twenty-two years", "22 years", "en"), (0, 3));
        assert_eq!(word_errors("dix-sept ans", "17 ans", "fr"), (0, 2));
        assert_eq!(word_errors("alle nove", "alle 9", "it"), (0, 2));
        assert_eq!(word_errors("las ocho y media", "las 8 y media", "es"), (0, 4));
        assert_eq!(word_errors("six heures", "sept heures", "fr"), (1, 2));
    }

    #[test]
    fn english_contractions_count_as_their_long_form() {
        assert_eq!(word_errors("Where is the nearest pharmacy?", "Where's the nearest pharmacy?", "en"), (0, 5));
        assert_eq!(word_errors("I've been learning", "I have been learning", "en"), (0, 4));
        assert_eq!(
            word_errors("Don't worry, it's not your fault.", "Do not worry, it is not your fault.", "en"),
            (0, 8)
        );
        assert_eq!(word_errors("I can't find them", "I cannot find them", "en"), (0, 5));
        assert_eq!(word_errors("We're thinking", "Who thinking", "en"), (2, 3));
        assert_eq!(word_errors("at my grandmother's", "at my grandmother's", "en"), (0, 4));
    }

    #[test]
    fn chrf_rewards_closeness() {
        let score = |reference: &str, hypothesis: &str| {
            let mut chrf = Chrf::default();
            chrf.add(reference, hypothesis);
            chrf.score()
        };
        assert!((score("Hello, how are you?", "hello, how are you?") - 100.0).abs() < 1e-9);
        assert_eq!(score("Hello", ""), 0.0);
        let near = score("I'm sorry, I'm late.", "I am sorry, I'm late.");
        let far = score("I'm sorry, I'm late.", "I thought I was going to die.");
        assert!(near > 70.0 && far < 30.0, "{near} {far}");
    }

    #[test]
    fn chrf_pools_statistics_across_sentences() {
        let mut both = Chrf::default();
        both.add("abcdef", "abcdef");
        both.add("ghijkl", "zzzzzz");
        let mut first = Chrf::default();
        first.add("abcdef", "abcdef");
        let mut second = Chrf::default();
        second.add("ghijkl", "zzzzzz");
        first.merge(&second);
        assert!((both.score() - first.score()).abs() < 1e-9);
        assert!((both.score() - 50.0).abs() < 1e-9);
    }

    #[test]
    fn chrf_matches_sacrebleu() {
        let pairs = [
            ("I'm sorry, I'm late.", "I am sorry, I'm late.", 86.9912),
            ("Hello, how are you?", "hello how are you", 66.4071),
            ("It's raining cats and dogs out there.", "I thought I was going to die.", 14.3336),
            ("My sister lives in Lyon with her two children.", "My sister lives in Lyons with her 2 kids.", 61.8791),
        ];
        let mut corpus = Chrf::default();
        for (reference, hypothesis, expected) in pairs {
            let mut chrf = Chrf::default();
            chrf.add(reference, hypothesis);
            assert!((chrf.score() - expected).abs() < 1e-3, "{hypothesis}: {}", chrf.score());
            corpus.merge(&chrf);
        }
        assert!((corpus.score() - 52.2341).abs() < 1e-3, "{}", corpus.score());
    }

    #[test]
    fn percentiles_use_nearest_rank() {
        assert_eq!(percentile(vec![], 0.5), 0);
        assert_eq!(percentile(vec![30, 10, 20], 0.5), 20);
        assert_eq!(percentile((1..=10).collect(), 0.9), 9);
        assert_eq!(percentile(vec![5], 0.9), 5);
    }

    #[test]
    fn the_simulated_engine_runs_a_set_end_to_end() {
        let engine = Engine::sim();
        let mut cases = parse_set("en-fr-01 | en | fr | hello world | bonjour monde\nen-fr-02 | en | fr | thanks friend | merci ami\nfr-en-01 | fr | en | bonjour | hello\nfr-de-01 | fr | de | bonjour | hallo\n").unwrap();
        let skipped = warm_up(&engine, &mut cases);
        assert_eq!(skipped.len(), 1);
        assert!(skipped[0].starts_with("fr-de: "), "{skipped:?}");
        assert_eq!(cases.len(), 3);
        let outcomes: Vec<Outcome> = cases.iter().map(|case| measure(&engine, case, None).unwrap()).collect();
        assert!(outcomes.iter().all(|o| o.word_errors.0 == 0 && !o.recorded), "{outcomes:?}");
        assert!(outcomes.iter().all(|o| o.voice_ms <= o.total_ms() + 1), "{outcomes:?}");
        let rows = summaries(&outcomes);
        assert_eq!(rows.iter().map(|r| r.label.as_str()).collect::<Vec<_>>(), ["en-fr", "fr-en", "all"]);
        assert_eq!(rows[2].count, 3);
        assert!((rows[0].chrf - 100.0).abs() < 1e-9);
        assert!(table(&rows).contains("en-fr"));
        assert!(misheard(&outcomes).is_empty());
        assert!(json("sim", "small", "opus", &rows, &outcomes).contains("\"pair\":\"all\""));
        assert!(mistranslated(&outcomes).is_empty());
    }

    #[test]
    fn recordings_replace_synthesized_speech() {
        let engine = Engine::sim();
        let dir = std::env::temp_dir().join(format!("ambivox-bench-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let audio = engine.synthesize("thanks", "en").unwrap();
        let mut file = File::create(dir.join("en-fr-01.wav")).unwrap();
        wav::write(&mut file, &audio.samples, audio.sample_rate).unwrap();
        drop(file);

        let case = parse_set("en-fr-01 | en | fr | hello | bonjour\n").unwrap().remove(0);
        let outcome = measure(&engine, &case, Some(&dir)).unwrap();
        std::fs::remove_dir_all(&dir).unwrap();
        assert!(outcome.recorded);
        assert_eq!(outcome.heard, "thanks");
        assert_eq!(outcome.word_errors, (1, 1));
        assert!(misheard(std::slice::from_ref(&outcome)).contains("heard: thanks"));
        let far = mistranslated(&[outcome]);
        assert!(far.contains("got:       merci") && far.contains("reference: bonjour"), "{far}");
    }

    #[test]
    fn json_strings_are_escaped() {
        assert_eq!(quoted("a \"b\"\\\n"), "\"a \\\"b\\\"\\\\\\n\"");
    }
}
