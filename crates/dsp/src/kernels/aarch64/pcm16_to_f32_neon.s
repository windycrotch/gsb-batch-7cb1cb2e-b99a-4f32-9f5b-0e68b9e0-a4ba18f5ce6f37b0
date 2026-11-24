    lsr x3, x2, #3
    cbz x3, 3f
2:
    ldr q0, [x0], #16
    sxtl v1.4s, v0.4h
    sxtl2 v2.4s, v0.8h
    scvtf v1.4s, v1.4s, #15
    scvtf v2.4s, v2.4s, #15
    stp q1, q2, [x1], #32
    subs x3, x3, #1
    b.ne 2b
3:
    and x2, x2, #7
    cbz x2, 5f
4:
    ldrsh w3, [x0], #2
    scvtf s0, w3, #15
    str s0, [x1], #4
    subs x2, x2, #1
    b.ne 4b
5:
    ret
