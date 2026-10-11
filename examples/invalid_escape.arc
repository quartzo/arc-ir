; invalid_escape.arc - a region created in a function dies at its RET, so a
; pointer into it must not be stored into memory that outlives the function.
; Here the callee stores a pointer into its local region into the caller's
; struct, which survives the call: the verifier rejects it.
struct Box
    p: ptr([i64; 8])
endstruct

func main
    s0 = ARENA_CREATE DRAM_LOCAL, 256
    s1 = ARENA_ALLOC s0, Box
    s2 = ARENA_ALLOC s0, [i64; 8]
    s1.p = s2
    r1 = s1
    CALL fill
    RET
endfunc

func fill(r1: ptr(Box))
    r2 = ARENA_CREATE DRAM_LOCAL, 64
    r3 = ARENA_ALLOC r2, [i64; 8]
    r1.p = r3
    RET
endfunc
