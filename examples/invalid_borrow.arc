; invalid_borrow.arc - the verifier must reject destroying a borrowed arena.
func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 64
    CALL consume
    ARENA_DESTROY r1
    RET

func consume(r1: arena):
    ARENA_DESTROY r1
    RET
