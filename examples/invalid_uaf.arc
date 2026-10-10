; invalid_uaf.arc - the verifier must reject this use-after-destroy.
entry:
    r1 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC r1, [i64; 4]
    r3 = 7
    ARENA_DESTROY r1
    STORE [r2], r3
    RET
