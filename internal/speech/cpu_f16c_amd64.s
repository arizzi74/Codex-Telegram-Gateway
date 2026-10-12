#include "textflag.h"

TEXT ·hasF16C(SB), NOSPLIT, $0-1
	MOVL $1, AX
	CPUID
	SHRL $29, CX
	ANDL $1, CX
	MOVB CL, ret+0(FP)
	RET
