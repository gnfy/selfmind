//go:build darwin

#include "textflag.h"

TEXT libc_sandbox_check_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sandbox_check(SB)

GLOBL	·libc_sandbox_check_trampoline_addr(SB), RODATA, $8
DATA	·libc_sandbox_check_trampoline_addr(SB)/8, $libc_sandbox_check_trampoline<>(SB)
