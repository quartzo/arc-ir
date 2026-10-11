; ptr.arc - pass a data pointer to a function.
; The arena handle survives the call, so it lives in a preserved register (s0).
func main -> i64
    s0 = ARENA_CREATE DRAM_LOCAL, 64
    r2 = ARENA_ALLOC s0, [i64; 8]
    r3 = 42
    STORE [r2], r3
    r1 = r2
    CALL read
    PRINT r0
    RET r0
endfunc

func read(r1: ptr([i64; 8])) -> i64
    r0 = LOAD [r1]
    RET r0
endfunc
