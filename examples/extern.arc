; FFI: calling a foreign (C) function.
;
; An `extern` declaration names a function that is not defined in ARC. It has no
; body; its typed signature is a contract the verifier trusts, and the backend
; calls the linked symbol through the System V AMD64 ABI:
;
;   - integer/pointer arguments go in %rdi..%r9, taken from r1..r6;
;   - float arguments go in %xmm0..%xmm7, taken from v0..v7;
;   - a scalar or pointer result comes back in r0, a float result in v0.
;
; Run:  arcc build examples/extern.arc -run

extern "labs" func labs(r1: i64) -> i64
extern "getpid" func getpid() -> i64

func main -> i64
    r1 = -7
    CALL labs
    PRINT r0                ; 7

    CALL getpid
    PRINT r0                ; the process id

    RET
endfunc
