; invalid_oob.arc - the verifier must reject this out-of-bounds store.
entry:
    r1 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC r1, [i64; 2]
    r3 = 7
    STORE [r2 + 12], r3
    RET
