    movi v0.4s, #0
    movi v1.4s, #0
    movi v2.4s, #0
    movi v3.4s, #0
    lsr x3, x2, #4
    cbz x3, 3f
2:
    ldp q4, q5, [x0], #32
    ldp q6, q7, [x0], #32
    ldp q16, q17, [x1], #32
    ldp q18, q19, [x1], #32
    fmla v0.4s, v4.4s, v16.4s
    fmla v1.4s, v5.4s, v17.4s
    fmla v2.4s, v6.4s, v18.4s
    fmla v3.4s, v7.4s, v19.4s
    subs x3, x3, #1
    b.ne 2b
3:
    fadd v0.4s, v0.4s, v1.4s
    fadd v2.4s, v2.4s, v3.4s
    fadd v0.4s, v0.4s, v2.4s
    and x2, x2, #15
    lsr x3, x2, #2
    cbz x3, 5f
4:
    ldr q4, [x0], #16
    ldr q16, [x1], #16
    fmla v0.4s, v4.4s, v16.4s
    subs x3, x3, #1
    b.ne 4b
5:
    faddp v0.4s, v0.4s, v0.4s
    faddp s0, v0.2s
    and x2, x2, #3
    cbz x2, 7f
6:
    ldr s4, [x0], #4
    ldr s16, [x1], #4
    fmadd s0, s4, s16, s0
    subs x2, x2, #1
    b.ne 6b
7:
    ret
