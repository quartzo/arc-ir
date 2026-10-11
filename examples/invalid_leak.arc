; invalid_leak.arc - the verifier must reject an arena that is never released.
func main
    r1 = ARENA_CREATE DRAM_LOCAL, 64
    r2 = ARENA_ALLOC r1, [i64; 8]
    r3 = 1
    STORE [r2], r3
    RET
endfunc
