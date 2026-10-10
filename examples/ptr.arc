; ptr.arc - pass a data pointer to a function.
func main:
    r5 = ARENA_CREATE DRAM_LOCAL, 64
    r2 = ARENA_ALLOC r5, 64
    r3 = 42
    STORE [r2], r3
    r1 = r2
    CALL read
    PRINT r0
    ARENA_DESTROY r5
    RET r0

func read(r1: ptr(64)):
    r0 = LOAD [r1]
    RET r0
