; typed_mem.arc - store a data pointer in memory and load it back.
; The verifier tracks the type of the cell at [r2], so r5 comes back as a
; data pointer (not an unknown scalar) and [r5] is a checked access.
func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC r1, 128
    r3 = ARENA_ALLOC r1, 8
    r4 = 99
    STORE [r3], r4          ; *r3 = 99
    STORE [r2], r3          ; *(ptr*)r2 = r3
    r5 = LOAD [r2]          ; r5 is a data pointer again
    r6 = LOAD [r5]          ; r6 = *r5 = 99
    PRINT r6
    ARENA_DESTROY r1
    RET r6
