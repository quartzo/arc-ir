; region.arc - a region is created and released by the same function: the caller
; creates it and it is released automatically when the caller returns. A callee
; borrows it (an arena parameter) and allocates from it.
func main
    s0 = ARENA_CREATE DRAM_LOCAL, 64
    r1 = s0
    CALL fill
    r0 = 0
    RET r0
endfunc

func fill(r1: arena)
    r2 = ARENA_ALLOC r1, [i64; 8]
    r3 = 5
    STORE [r2], r3
    RET
endfunc
