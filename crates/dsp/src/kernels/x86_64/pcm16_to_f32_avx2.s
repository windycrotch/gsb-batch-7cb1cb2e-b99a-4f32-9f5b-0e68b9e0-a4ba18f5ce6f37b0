    mov eax, 0x38000000
    vmovd xmm7, eax
    vbroadcastss ymm7, xmm7
    mov rcx, rdx
    shr rcx, 3
    jz 3f
2:
    vpmovsxwd ymm0, xmmword ptr [rdi]
    vcvtdq2ps ymm0, ymm0
    vmulps ymm0, ymm0, ymm7
    vmovups ymmword ptr [rsi], ymm0
    add rdi, 16
    add rsi, 32
    dec rcx
    jnz 2b
3:
    and rdx, 7
    jz 5f
4:
    movsx eax, word ptr [rdi]
    vcvtsi2ss xmm0, xmm0, eax
    vmulss xmm0, xmm0, xmm7
    vmovss dword ptr [rsi], xmm0
    add rdi, 2
    add rsi, 4
    dec rdx
    jnz 4b
5:
    vzeroupper
    ret
