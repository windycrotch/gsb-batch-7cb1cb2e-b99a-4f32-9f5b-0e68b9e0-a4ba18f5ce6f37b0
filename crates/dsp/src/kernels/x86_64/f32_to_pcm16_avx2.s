    mov eax, 0x47000000
    vmovd xmm5, eax
    vbroadcastss ymm5, xmm5
    mov eax, 0x46fffe00
    vmovd xmm6, eax
    vbroadcastss ymm6, xmm6
    mov eax, 0xc7000000
    vmovd xmm7, eax
    vbroadcastss ymm7, xmm7
    mov rcx, rdx
    shr rcx, 3
    jz 3f
2:
    vmovups ymm0, ymmword ptr [rdi]
    vcmpordps ymm1, ymm0, ymm0
    vandps ymm0, ymm0, ymm1
    vmulps ymm0, ymm0, ymm5
    vminps ymm0, ymm0, ymm6
    vmaxps ymm0, ymm0, ymm7
    vcvtps2dq ymm0, ymm0
    vextracti128 xmm1, ymm0, 1
    vpackssdw xmm0, xmm0, xmm1
    vmovdqu xmmword ptr [rsi], xmm0
    add rdi, 32
    add rsi, 16
    dec rcx
    jnz 2b
3:
    and rdx, 7
    jz 5f
4:
    vmovss xmm0, dword ptr [rdi]
    vcmpordss xmm1, xmm0, xmm0
    vandps xmm0, xmm0, xmm1
    vmulss xmm0, xmm0, xmm5
    vminss xmm0, xmm0, xmm6
    vmaxss xmm0, xmm0, xmm7
    vcvtss2si eax, xmm0
    mov word ptr [rsi], ax
    add rdi, 4
    add rsi, 2
    dec rdx
    jnz 4b
5:
    vzeroupper
    ret
