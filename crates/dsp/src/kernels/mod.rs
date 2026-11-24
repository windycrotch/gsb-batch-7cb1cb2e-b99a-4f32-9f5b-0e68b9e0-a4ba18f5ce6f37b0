pub mod scalar;

#[cfg(all(feature = "asm", target_arch = "x86_64"))]
mod x86_64;
#[cfg(all(feature = "asm", target_arch = "x86_64"))]
use x86_64 as imp;

#[cfg(all(feature = "asm", target_arch = "aarch64"))]
mod aarch64;
#[cfg(all(feature = "asm", target_arch = "aarch64"))]
use aarch64 as imp;

#[cfg(not(all(feature = "asm", any(target_arch = "x86_64", target_arch = "aarch64"))))]
use scalar as imp;

pub fn backend() -> &'static str {
    #[cfg(all(feature = "asm", target_arch = "x86_64"))]
    {
        if x86_64::has_avx2_fma() { "avx2+fma" } else { "sse2" }
    }
    #[cfg(all(feature = "asm", target_arch = "aarch64"))]
    {
        "neon"
    }
    #[cfg(not(all(feature = "asm", any(target_arch = "x86_64", target_arch = "aarch64"))))]
    {
        "scalar"
    }
}

pub fn dot(a: &[f32], b: &[f32]) -> f32 {
    assert_eq!(a.len(), b.len());
    imp::dot(a, b)
}

pub fn energy(x: &[f32]) -> f32 {
    imp::dot(x, x)
}

pub fn pcm16_to_f32(src: &[i16], dst: &mut [f32]) {
    assert_eq!(src.len(), dst.len());
    imp::pcm16_to_f32(src, dst)
}

pub fn f32_to_pcm16(src: &[f32], dst: &mut [i16]) {
    assert_eq!(src.len(), dst.len());
    imp::f32_to_pcm16(src, dst)
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Lcg(u64);

    impl Lcg {
        fn next_f32(&mut self) -> f32 {
            self.0 = self.0.wrapping_mul(6364136223846793005).wrapping_add(1442695040888963407);
            ((self.0 >> 40) as f32 / (1u64 << 24) as f32) * 2.0 - 1.0
        }
        fn vec(&mut self, n: usize, scale: f32) -> Vec<f32> {
            (0..n).map(|_| self.next_f32() * scale).collect()
        }
    }

    const LENGTHS: [usize; 16] = [0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 31, 33, 96, 127, 1025];

    const EDGE_CASES: [f32; 14] = [
        0.0,
        -0.0,
        1.0,
        -1.0,
        0.999_99,
        1.5,
        -1.5,
        f32::INFINITY,
        f32::NEG_INFINITY,
        f32::NAN,
        0.5 / 32768.0,
        1.5 / 32768.0,
        -2.5 / 32768.0,
        1e30,
    ];

    fn assert_close(got: f32, want: f32, a: &[f32], b: &[f32]) {
        let magnitude: f32 = a.iter().zip(b).map(|(x, y)| (x * y).abs()).sum();
        assert!((got - want).abs() <= 1e-5 * magnitude.max(1.0), "got {got}, want {want}, n = {}", a.len());
    }

    #[test]
    fn dot_matches_scalar() {
        let mut rng = Lcg(1);
        for n in LENGTHS {
            let (a, b) = (rng.vec(n, 1.0), rng.vec(n, 1.0));
            assert_close(dot(&a, &b), scalar::dot(&a, &b), &a, &b);
        }
    }

    #[test]
    fn dot_handles_unaligned_slices() {
        let mut rng = Lcg(2);
        let (a, b) = (rng.vec(200, 1.0), rng.vec(200, 1.0));
        for offset in 0..9 {
            let (x, y) = (&a[offset..], &b[offset..]);
            assert_close(dot(x, y), scalar::dot(x, y), x, y);
        }
    }

    #[test]
    fn energy_of_known_signal() {
        let x = vec![0.5f32; 64];
        assert!((energy(&x) - 16.0).abs() < 1e-4);
    }

    #[test]
    fn pcm16_to_f32_matches_scalar() {
        let mut rng = Lcg(3);
        for n in LENGTHS {
            let mut src: Vec<i16> = rng.vec(n, 32767.0).iter().map(|v| *v as i16).collect();
            if n > 2 {
                src[0] = i16::MIN;
                src[1] = i16::MAX;
            }
            let (mut got, mut want) = (vec![0.0; n], vec![0.0; n]);
            pcm16_to_f32(&src, &mut got);
            scalar::pcm16_to_f32(&src, &mut want);
            assert_eq!(got, want, "n = {n}");
        }
    }

    #[test]
    fn f32_to_pcm16_matches_scalar() {
        let mut rng = Lcg(4);
        for n in LENGTHS {
            let mut src = rng.vec(n, 1.2);
            for (slot, edge) in src.iter_mut().zip(EDGE_CASES) {
                *slot = edge;
            }
            src.reverse();
            for (slot, edge) in src.iter_mut().zip(EDGE_CASES) {
                *slot = edge;
            }
            let (mut got, mut want) = (vec![0i16; n], vec![0i16; n]);
            f32_to_pcm16(&src, &mut got);
            scalar::f32_to_pcm16(&src, &mut want);
            assert_eq!(got, want, "n = {n}");
        }
    }

    #[test]
    fn f32_to_pcm16_saturates_and_rounds_to_even() {
        let src = [2.0, -2.0, f32::NAN, 0.5 / 32768.0, 1.5 / 32768.0, 2.5 / 32768.0, 1.0, -1.0];
        let mut dst = [0i16; 8];
        f32_to_pcm16(&src, &mut dst);
        assert_eq!(dst, [32767, -32768, 0, 0, 2, 2, 32767, -32768]);
    }

    #[test]
    fn pcm16_roundtrip_is_lossless() {
        let src: Vec<i16> = (i16::MIN..=i16::MAX).step_by(7).collect();
        let mut float = vec![0.0f32; src.len()];
        let mut back = vec![0i16; src.len()];
        pcm16_to_f32(&src, &mut float);
        f32_to_pcm16(&float, &mut back);
        assert_eq!(src, back);
    }

    #[cfg(all(feature = "asm", target_arch = "x86_64"))]
    #[test]
    fn sse2_paths_match_scalar() {
        use super::x86_64::raw;
        let mut rng = Lcg(5);
        for n in LENGTHS {
            let (a, b) = (rng.vec(n, 1.0), rng.vec(n, 1.0));
            assert_close(raw::dot_sse2(&a, &b), scalar::dot(&a, &b), &a, &b);

            let ints: Vec<i16> = rng.vec(n, 32767.0).iter().map(|v| *v as i16).collect();
            let (mut got, mut want) = (vec![0.0; n], vec![0.0; n]);
            raw::pcm16_to_f32_sse2(&ints, &mut got);
            scalar::pcm16_to_f32(&ints, &mut want);
            assert_eq!(got, want);

            let mut floats = rng.vec(n, 1.2);
            for (slot, edge) in floats.iter_mut().zip(EDGE_CASES) {
                *slot = edge;
            }
            let (mut got, mut want) = (vec![0i16; n], vec![0i16; n]);
            raw::f32_to_pcm16_sse2(&floats, &mut got);
            scalar::f32_to_pcm16(&floats, &mut want);
            assert_eq!(got, want);
        }
    }
}
