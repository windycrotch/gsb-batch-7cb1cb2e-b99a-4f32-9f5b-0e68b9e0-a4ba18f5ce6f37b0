    vxorps ymm0, ymm0, ymm0
    vxorps ymm1, ymm1, ymm1
    vxorps ymm2, ymm2, ymm2
    vxorps ymm3, ymm3, ymm3
    mov rcx, rdx
    shr rcx, 5
    jz 3f
2:
    vmovups ymm4, ymmword ptr [rdi]
    vmovups ymm5, ymmword ptr [rdi + 32]
    vmovups ymm6, ymmword ptr [rdi + 64]
    vmovups ymm7, ymmword ptr [rdi + 96]
    vfmadd231ps ymm0, ymm4, ymmword ptr [rsi]
    vfmadd231ps ymm1, ymm5, ymmword ptr [rsi + 32]
    vfmadd231ps ymm2, ymm6, ymmword ptr [rsi + 64]
    vfmadd231ps ymm3, ymm7, ymmword ptr [rsi + 96]
    add rdi, 128
    add rsi, 128
    dec rcx
    jnz 2b
3:
    vaddps ymm0, ymm0, ymm1
    vaddps ymm2, ymm2, ymm3
    vaddps ymm0, ymm0, ymm2
    and rdx, 31
    mov rcx, rdx
    shr rcx, 3
    jz 5f
4:
    vmovups ymm4, ymmword ptr [rdi]
    vfmadd231ps ymm0, ymm4, ymmword ptr [rsi]
    add rdi, 32
    add rsi, 32
    dec rcx
    jnz 4b
5:
    vextractf128 xmm1, ymm0, 1
    vaddps xmm0, xmm0, xmm1
    vhaddps xmm0, xmm0, xmm0
    vhaddps xmm0, xmm0, xmm0
    and rdx, 7
    jz 7f
6:
    vmovss xmm1, dword ptr [rdi]
    vfmadd231ss xmm0, xmm1, dword ptr [rsi]
    add rdi, 4
    add rsi, 4
    dec rdx
    jnz 6b
7:
    vzeroupper
    ret
