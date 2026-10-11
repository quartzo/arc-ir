; A control-flow join may not merge two different arena handles into one
; register: the value would be "one of several arenas", for which no single
; capacity can be accounted. Rejected:
; "arena handle may be one of several arenas at this join".
func main -> i64
    r1 = 0
    JEQ r1, 0, b
    s0 = ARENA_CREATE DRAM_LOCAL, 16
    JMP done
b:
    s0 = ARENA_CREATE DRAM_LOCAL, 16
done:
    r2 = ARENA_ALLOC s0, [i64; 1]
    RET
endfunc
