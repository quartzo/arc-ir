; invalid_ptr.arc - the verifier must reject passing a 16-byte allocation to a
; parameter declared ptr([i64; 8]).
func main
    r5 = ARENA_CREATE DRAM_LOCAL, 64
    r2 = ARENA_ALLOC r5, [i64; 2]
    r1 = r2
    CALL read
    RET r0
endfunc

func read(r1: ptr([i64; 8]))
    r0 = LOAD [r1]
    RET r0
endfunc
