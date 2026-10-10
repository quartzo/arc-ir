; own.arc - transfer arena ownership to a function that destroys it.
func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 64
    r2 = ARENA_ALLOC r1, [i64; 8]
    r3 = 5
    STORE [r2], r3
    r4 = LOAD [r2]
    PRINT r4
    CALL consume
    RET r4

func consume(r1: own arena):
    ARENA_DESTROY r1
    RET
