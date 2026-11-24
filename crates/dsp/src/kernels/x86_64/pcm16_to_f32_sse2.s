    mov eax, 0x38000000
    movd xmm7, eax
    shufps xmm7, xmm7, 0
    mov rcx, rdx
    shr rcx, 2
    jz 3f
2:
    movq xmm0, qword ptr [rdi]
    punpcklwd xmm0, xmm0
    psrad xmm0, 16
    cvtdq2ps xmm0, xmm0
    mulps xmm0, xmm7
    movups xmmword ptr [rsi], xmm0
    add rdi, 8
    add rsi, 16
    dec rcx
    jnz 2b
3:
    and rdx, 3
    jz 5f
4:
    movsx eax, word ptr [rdi]
    cvtsi2ss xmm0, eax
    mulss xmm0, xmm7
    movss dword ptr [rsi], xmm0
    add rdi, 2
    add rsi, 4
    dec rdx
    jnz 4b
5:
    ret
