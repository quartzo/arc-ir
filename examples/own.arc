; own.arc - transfer arena ownership to a function that destroys it.
; The value kept across the call lives in a preserved register (s0).
func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 64
    r2 = ARENA_ALLOC r1, [i64; 8]
    r3 = 5
    STORE [r2], r3
    s0 = LOAD [r2]
    PRINT s0
    CALL consume
    RET s0

func consume(r1: own arena):
    ARENA_DESTROY r1
    RET
