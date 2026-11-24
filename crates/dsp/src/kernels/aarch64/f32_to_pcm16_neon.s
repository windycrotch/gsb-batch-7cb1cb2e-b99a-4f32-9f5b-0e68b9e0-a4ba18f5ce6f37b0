    movi v7.4s, #0x47, lsl #24
    lsr x3, x2, #3
    cbz x3, 3f
2:
    ldp q0, q1, [x0], #32
    fmul v0.4s, v0.4s, v7.4s
    fmul v1.4s, v1.4s, v7.4s
    fcvtns v0.4s, v0.4s
    fcvtns v1.4s, v1.4s
    sqxtn v0.4h, v0.4s
    sqxtn2 v0.8h, v1.4s
    str q0, [x1], #16
    subs x3, x3, #1
    b.ne 2b
3:
    and x2, x2, #7
    cbz x2, 5f
4:
    ldr s0, [x0], #4
    fmul s0, s0, s7
    fcvtns s0, s0
    sqxtn h0, s0
    str h0, [x1], #2
    subs x2, x2, #1
    b.ne 4b
5:
    ret
