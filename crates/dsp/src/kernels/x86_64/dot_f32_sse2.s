    xorps xmm0, xmm0
    xorps xmm1, xmm1
    mov rcx, rdx
    shr rcx, 3
    jz 3f
2:
    movups xmm2, xmmword ptr [rdi]
    movups xmm3, xmmword ptr [rsi]
    movups xmm4, xmmword ptr [rdi + 16]
    movups xmm5, xmmword ptr [rsi + 16]
    mulps xmm2, xmm3
    mulps xmm4, xmm5
    addps xmm0, xmm2
    addps xmm1, xmm4
    add rdi, 32
    add rsi, 32
    dec rcx
    jnz 2b
3:
    addps xmm0, xmm1
    movhlps xmm1, xmm0
    addps xmm0, xmm1
    movaps xmm1, xmm0
    shufps xmm1, xmm1, 1
    addss xmm0, xmm1
    and rdx, 7
    jz 5f
4:
    movss xmm1, dword ptr [rdi]
    mulss xmm1, dword ptr [rsi]
    addss xmm0, xmm1
    add rdi, 4
    add rsi, 4
    dec rdx
    jnz 4b
5:
    ret
