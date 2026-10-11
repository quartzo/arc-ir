; ret_ptr.arc - the caller owns the region and passes it down; a callee returns
; a data pointer into it. The region outlives the call, so the pointer is valid.
; The arena handle survives the call, so it lives in a preserved register (s0).
func main -> i64
    s0 = ARENA_CREATE DRAM_LOCAL, 64
    r1 = s0                 ; pass the borrowed region
    CALL make               ; make(r1) -> ptr([i64; 8])
    r2 = r0
    r3 = 99
    STORE [r2], r3
    r4 = LOAD [r2]
    PRINT r4
    RET r4
endfunc

func make(r1: arena) -> ptr([i64; 8])
    r0 = ARENA_ALLOC r1, [i64; 8]
    RET r0
endfunc
