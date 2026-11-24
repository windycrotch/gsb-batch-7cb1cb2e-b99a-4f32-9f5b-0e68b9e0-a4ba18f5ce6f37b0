pub fn dot(a: &[f32], b: &[f32]) -> f32 {
    let (chunks_a, rest_a) = a.as_chunks::<8>();
    let (chunks_b, rest_b) = b.as_chunks::<8>();
    let mut acc = [0.0f32; 8];
    for (ca, cb) in chunks_a.iter().zip(chunks_b) {
        for i in 0..8 {
            acc[i] += ca[i] * cb[i];
        }
    }
    let mut sum: f32 = acc.iter().sum();
    for (x, y) in rest_a.iter().zip(rest_b) {
        sum += x * y;
    }
    sum
}

pub fn pcm16_to_f32(src: &[i16], dst: &mut [f32]) {
    for (d, s) in dst.iter_mut().zip(src) {
        *d = f32::from(*s) / 32768.0;
    }
}

pub fn f32_to_pcm16(src: &[f32], dst: &mut [i16]) {
    for (d, s) in dst.iter_mut().zip(src) {
        let scaled = (*s * 32768.0).round_ties_even();
        *d = if scaled.is_nan() { 0 } else { scaled.clamp(-32768.0, 32767.0) as i16 };
    }
}
