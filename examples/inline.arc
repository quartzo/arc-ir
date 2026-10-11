; inline.arc - a loop body delegated to a function marked `inline`. The call is
; inlined, so there is no call overhead; the function's region is still released
; at the point that was its return.
inline func step(r1: i64) -> i64
    r2 = ARENA_CREATE DRAM_LOCAL, 64
    r3 = ARENA_ALLOC r2, [i64; 8]
    STORE [r3], r1
    r4 = LOAD [r3]
    r0 = ADD r4, 1
    RET r0
endfunc

func main -> i64
    s0 = 0
    r0 = 0
loop:
    JGE s0, 5, done
    r1 = s0
    CALL step
    s0 = ADD s0, 1
    JMP loop
done:
    PRINT r0
    RET r0
endfunc
