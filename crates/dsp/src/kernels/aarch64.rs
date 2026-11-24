use core::arch::naked_asm;

macro_rules! kernel {
    ($name:ident, $file:literal, ($($arg:ident: $ty:ty),*) $(-> $ret:ty)?) => {
        #[unsafe(naked)]
        unsafe extern "C" fn $name($($arg: $ty),*) $(-> $ret)? {
            naked_asm!(include_str!($file))
        }
    };
}

kernel!(dot_f32_neon, "aarch64/dot_f32_neon.s", (a: *const f32, b: *const f32, n: usize) -> f32);
kernel!(pcm16_to_f32_neon, "aarch64/pcm16_to_f32_neon.s", (src: *const i16, dst: *mut f32, n: usize));
kernel!(f32_to_pcm16_neon, "aarch64/f32_to_pcm16_neon.s", (src: *const f32, dst: *mut i16, n: usize));

pub fn dot(a: &[f32], b: &[f32]) -> f32 {
    // SAFETY: both slices hold `a.len()` readable floats; NEON is part of the AArch64 baseline.
    unsafe { dot_f32_neon(a.as_ptr(), b.as_ptr(), a.len()) }
}

pub fn pcm16_to_f32(src: &[i16], dst: &mut [f32]) {
    // SAFETY: `dst` has room for `src.len()` floats (checked by the caller).
    unsafe { pcm16_to_f32_neon(src.as_ptr(), dst.as_mut_ptr(), src.len()) }
}

pub fn f32_to_pcm16(src: &[f32], dst: &mut [i16]) {
    // SAFETY: `dst` has room for `src.len()` samples (checked by the caller).
    unsafe { f32_to_pcm16_neon(src.as_ptr(), dst.as_mut_ptr(), src.len()) }
}
