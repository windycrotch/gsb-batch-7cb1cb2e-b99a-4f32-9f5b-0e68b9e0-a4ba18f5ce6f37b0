    mov eax, 0x47000000
    movd xmm5, eax
    shufps xmm5, xmm5, 0
    mov eax, 0x46fffe00
    movd xmm6, eax
    shufps xmm6, xmm6, 0
    mov eax, 0xc7000000
    movd xmm7, eax
    shufps xmm7, xmm7, 0
    mov rcx, rdx
    shr rcx, 2
    jz 3f
2:
    movups xmm0, xmmword ptr [rdi]
    movaps xmm1, xmm0
    cmpordps xmm1, xmm1
    andps xmm0, xmm1
    mulps xmm0, xmm5
    minps xmm0, xmm6
    maxps xmm0, xmm7
    cvtps2dq xmm0, xmm0
    packssdw xmm0, xmm0
    movq qword ptr [rsi], xmm0
    add rdi, 16
    add rsi, 8
    dec rcx
    jnz 2b
3:
    and rdx, 3
    jz 5f
4:
    movss xmm0, dword ptr [rdi]
    movaps xmm1, xmm0
    cmpordss xmm1, xmm1
    andps xmm0, xmm1
    mulss xmm0, xmm5
    minss xmm0, xmm6
    maxss xmm0, xmm7
    cvtss2si eax, xmm0
    mov word ptr [rsi], ax
    add rdi, 4
    add rsi, 2
    dec rdx
    jnz 4b
5:
    ret
