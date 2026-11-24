use crate::kernels;

const ZERO_CROSSINGS: usize = 16;
const KAISER_BETA: f64 = 9.0;
const PASSBAND: f64 = 0.92;

pub struct Resampler {
    up: usize,
    down: usize,
    taps: usize,
    phases: Vec<f32>,
    buffer: Vec<f32>,
    position: usize,
}

impl Resampler {
    pub fn new(from_hz: u32, to_hz: u32) -> Self {
        assert!(from_hz > 0 && to_hz > 0);
        let divisor = gcd(from_hz as usize, to_hz as usize);
        let up = to_hz as usize / divisor;
        let down = from_hz as usize / divisor;

        let stretch = down.div_ceil(up).max(1);
        let taps = (2 * ZERO_CROSSINGS * stretch).next_multiple_of(8);
        let length = taps * up;
        let cutoff = PASSBAND * 0.5 / up.max(down) as f64;
        let center = (length - 1) as f64 / 2.0;
        let norm = bessel_i0(KAISER_BETA);

        let prototype: Vec<f64> = (0..length)
            .map(|i| {
                let t = i as f64 - center;
                let sinc = if t == 0.0 {
                    2.0 * cutoff
                } else {
                    (2.0 * std::f64::consts::PI * cutoff * t).sin() / (std::f64::consts::PI * t)
                };
                let r = t / (center + 1.0);
                let window = bessel_i0(KAISER_BETA * (1.0 - r * r).max(0.0).sqrt()) / norm;
                sinc * window * up as f64
            })
            .collect();

        let mut phases = vec![0.0f32; length];
        for phase in 0..up {
            let gain: f64 = (0..taps).map(|k| prototype[phase + k * up]).sum();
            for slot in 0..taps {
                phases[phase * taps + slot] = (prototype[phase + (taps - 1 - slot) * up] / gain) as f32;
            }
        }

        Self { up, down, taps, phases, buffer: vec![0.0; taps - 1], position: 0 }
    }

    pub fn ratio(&self) -> (usize, usize) {
        (self.up, self.down)
    }

    pub fn delay_seconds(&self, from_hz: u32) -> f64 {
        (self.taps - 1) as f64 / 2.0 / from_hz as f64
    }

    pub fn process(&mut self, input: &[f32], output: &mut Vec<f32>) {
        self.buffer.extend_from_slice(input);
        loop {
            let index = self.position / self.up;
            if index + self.taps > self.buffer.len() {
                break;
            }
            let phase = self.position % self.up;
            let coefficients = &self.phases[phase * self.taps..(phase + 1) * self.taps];
            output.push(kernels::dot(&self.buffer[index..index + self.taps], coefficients));
            self.position += self.down;
        }
        let consumed = (self.position / self.up).min(self.buffer.len());
        self.buffer.drain(..consumed);
        self.position -= consumed * self.up;
    }

    pub fn flush(&mut self, output: &mut Vec<f32>) {
        let tail = vec![0.0; self.taps / 2];
        self.process(&tail, output);
    }

    pub fn reset(&mut self) {
        self.buffer.clear();
        self.buffer.resize(self.taps - 1, 0.0);
        self.position = 0;
    }
}

pub fn resample(input: &[f32], from_hz: u32, to_hz: u32) -> Vec<f32> {
    if from_hz == to_hz {
        return input.to_vec();
    }
    let mut resampler = Resampler::new(from_hz, to_hz);
    let mut output = Vec::with_capacity(input.len() * to_hz as usize / from_hz as usize + 64);
    resampler.process(input, &mut output);
    resampler.flush(&mut output);
    output
}

fn gcd(mut a: usize, mut b: usize) -> usize {
    while b != 0 {
        (a, b) = (b, a % b);
    }
    a
}

fn bessel_i0(x: f64) -> f64 {
    let (mut sum, mut term, mut k) = (1.0, 1.0, 1.0);
    while term > 1e-12 * sum {
        term *= (x / (2.0 * k)) * (x / (2.0 * k));
        sum += term;
        k += 1.0;
    }
    sum
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sine(hz: f64, rate: u32, seconds: f64) -> Vec<f32> {
        (0..(rate as f64 * seconds) as usize)
            .map(|i| (2.0 * std::f64::consts::PI * hz * i as f64 / rate as f64).sin() as f32)
            .collect()
    }

    fn rms(x: &[f32]) -> f32 {
        (kernels::energy(x) / x.len() as f32).sqrt()
    }

    fn steady(x: &[f32]) -> &[f32] {
        &x[x.len() / 4..x.len() * 3 / 4]
    }

    #[test]
    fn ratios_are_reduced() {
        assert_eq!(Resampler::new(48000, 16000).ratio(), (1, 3));
        assert_eq!(Resampler::new(16000, 48000).ratio(), (3, 1));
        assert_eq!(Resampler::new(22050, 48000).ratio(), (320, 147));
    }

    #[test]
    fn output_length_follows_ratio() {
        for (from, to) in [(48000, 16000), (16000, 48000), (22050, 48000), (44100, 16000)] {
            let out = resample(&sine(440.0, from, 1.0), from, to);
            let expected = to as i64;
            assert!((out.len() as i64 - expected).abs() < 64, "{from}->{to}: {}", out.len());
        }
    }

    #[test]
    fn passband_tone_keeps_its_level() {
        for (from, to) in [(48000, 16000), (16000, 48000), (22050, 48000), (22050, 16000)] {
            for hz in [200.0, 1000.0, 3000.0, 6000.0] {
                let out = resample(&sine(hz, from, 0.5), from, to);
                let level = rms(steady(&out));
                assert!((level - std::f32::consts::FRAC_1_SQRT_2).abs() < 0.01, "{from}->{to} at {hz} Hz: rms {level}");
            }
        }
    }

    #[test]
    fn passband_tone_keeps_its_frequency() {
        let out = resample(&sine(1000.0, 48000, 0.5), 48000, 16000);
        let body = steady(&out);
        let crossings = body.windows(2).filter(|w| w[0] <= 0.0 && w[1] > 0.0).count();
        let expected = 1000.0 * body.len() as f64 / 16000.0;
        assert!((crossings as f64 - expected).abs() <= 1.0, "{crossings} vs {expected}");
    }

    #[test]
    fn decimation_rejects_aliases() {
        for hz in [9000.0, 12000.0, 20000.0] {
            let out = resample(&sine(hz, 48000, 0.5), 48000, 16000);
            let level = rms(steady(&out));
            assert!(level < 0.003, "{hz} Hz leaked at rms {level}");
        }
    }

    #[test]
    fn interpolation_rejects_images() {
        let out = resample(&sine(3000.0, 16000, 0.5), 16000, 48000);
        let body = steady(&out);
        let n = body.len() - body.len() % 16;
        let body = &body[..n];
        let w = 2.0 * std::f64::consts::PI * 3000.0 / 48000.0;
        let (mut a, mut b) = (0.0f64, 0.0f64);
        for (i, x) in body.iter().enumerate() {
            a += *x as f64 * (w * i as f64).sin();
            b += *x as f64 * (w * i as f64).cos();
        }
        let (a, b) = (2.0 * a / n as f64, 2.0 * b / n as f64);
        let residual: f64 = body
            .iter()
            .enumerate()
            .map(|(i, x)| {
                let e = *x as f64 - a * (w * i as f64).sin() - b * (w * i as f64).cos();
                e * e
            })
            .sum::<f64>()
            / n as f64;
        assert!((a.hypot(b) - 1.0).abs() < 0.01, "amplitude {}", a.hypot(b));
        assert!(residual.sqrt() < 0.002, "residual rms {}", residual.sqrt());
    }

    #[test]
    fn streaming_matches_one_shot() {
        let input = sine(700.0, 22050, 0.4);
        let mut whole = Vec::new();
        Resampler::new(22050, 48000).process(&input, &mut whole);

        let mut chunked = Vec::new();
        let mut resampler = Resampler::new(22050, 48000);
        let mut offset = 0;
        for size in [1usize, 7, 160, 441, 3, 1000].iter().cycle() {
            if offset >= input.len() {
                break;
            }
            let end = (offset + size).min(input.len());
            resampler.process(&input[offset..end], &mut chunked);
            offset = end;
        }
        assert_eq!(whole, chunked);
    }

    #[test]
    fn reset_restores_initial_state() {
        let input = sine(500.0, 48000, 0.1);
        let mut resampler = Resampler::new(48000, 16000);
        let (mut first, mut second) = (Vec::new(), Vec::new());
        resampler.process(&input, &mut first);
        resampler.reset();
        resampler.process(&input, &mut second);
        assert_eq!(first, second);
    }
}
