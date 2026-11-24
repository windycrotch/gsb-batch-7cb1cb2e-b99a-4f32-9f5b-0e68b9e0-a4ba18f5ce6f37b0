use core::arch::naked_asm;

macro_rules! kernel {
    ($name:ident, $file:literal, ($($arg:ident: $ty:ty),*) $(-> $ret:ty)?) => {
        #[unsafe(naked)]
        unsafe extern "sysv64" fn $name($($arg: $ty),*) $(-> $ret)? {
            naked_asm!(include_str!($file))
        }
    };
}

kernel!(dot_f32_sse2, "x86_64/dot_f32_sse2.s", (a: *const f32, b: *const f32, n: usize) -> f32);
kernel!(dot_f32_avx2, "x86_64/dot_f32_avx2.s", (a: *const f32, b: *const f32, n: usize) -> f32);
kernel!(pcm16_to_f32_sse2, "x86_64/pcm16_to_f32_sse2.s", (src: *const i16, dst: *mut f32, n: usize));
kernel!(pcm16_to_f32_avx2, "x86_64/pcm16_to_f32_avx2.s", (src: *const i16, dst: *mut f32, n: usize));
kernel!(f32_to_pcm16_sse2, "x86_64/f32_to_pcm16_sse2.s", (src: *const f32, dst: *mut i16, n: usize));
kernel!(f32_to_pcm16_avx2, "x86_64/f32_to_pcm16_avx2.s", (src: *const f32, dst: *mut i16, n: usize));

pub fn has_avx2_fma() -> bool {
    std::is_x86_feature_detected!("avx2") && std::is_x86_feature_detected!("fma")
}

pub fn dot(a: &[f32], b: &[f32]) -> f32 {
    // SAFETY: both slices hold `a.len()` readable floats; the AVX2 path runs only when the CPU reports it.
    unsafe {
        if has_avx2_fma() {
            dot_f32_avx2(a.as_ptr(), b.as_ptr(), a.len())
        } else {
            dot_f32_sse2(a.as_ptr(), b.as_ptr(), a.len())
        }
    }
}

pub fn pcm16_to_f32(src: &[i16], dst: &mut [f32]) {
    // SAFETY: `dst` has room for `src.len()` floats (checked by the caller).
    unsafe {
        if has_avx2_fma() {
            pcm16_to_f32_avx2(src.as_ptr(), dst.as_mut_ptr(), src.len())
        } else {
            pcm16_to_f32_sse2(src.as_ptr(), dst.as_mut_ptr(), src.len())
        }
    }
}

pub fn f32_to_pcm16(src: &[f32], dst: &mut [i16]) {
    // SAFETY: `dst` has room for `src.len()` samples (checked by the caller).
    unsafe {
        if has_avx2_fma() {
            f32_to_pcm16_avx2(src.as_ptr(), dst.as_mut_ptr(), src.len())
        } else {
            f32_to_pcm16_sse2(src.as_ptr(), dst.as_mut_ptr(), src.len())
        }
    }
}

#[cfg(test)]
pub(crate) mod raw {
    pub fn dot_sse2(a: &[f32], b: &[f32]) -> f32 {
        unsafe { super::dot_f32_sse2(a.as_ptr(), b.as_ptr(), a.len()) }
    }
    pub fn pcm16_to_f32_sse2(src: &[i16], dst: &mut [f32]) {
        unsafe { super::pcm16_to_f32_sse2(src.as_ptr(), dst.as_mut_ptr(), src.len()) }
    }
    pub fn f32_to_pcm16_sse2(src: &[f32], dst: &mut [i16]) {
        unsafe { super::f32_to_pcm16_sse2(src.as_ptr(), dst.as_mut_ptr(), src.len()) }
    }
}
