; region.arc - a region is created and released by the same function. A callee
; borrows it (an arena parameter) and allocates from it, and must not release it.
func main
    s0 = ARENA_CREATE DRAM_LOCAL, 64
    r1 = s0
    CALL fill
    ARENA_DESTROY s0
    r0 = 0
    RET r0
endfunc

func fill(r1: arena)
    r2 = ARENA_ALLOC r1, [i64; 8]
    r3 = 5
    STORE [r2], r3
    RET
endfunc
