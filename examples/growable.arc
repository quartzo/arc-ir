; growable.arc - an extensible arena that outgrows its first chunk.
; Chunks are 64 bytes; the second allocation forces a new chunk.
func main -> i64
    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)
    r2 = ARENA_ALLOC r1, [i64; 8]      ; fills chunk 1
    r3 = ARENA_ALLOC r1, [i64; 8]      ; forces a second chunk
    r4 = 77
    STORE [r3], r4
    r5 = LOAD [r3]
    PRINT r5
    RET r5
endfunc
