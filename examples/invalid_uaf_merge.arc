; invalid_uaf_merge.arc - a data pointer whose arena identity is lost at a
; control-flow join must be treated as possibly aliasing any arena. Destroying
; one of the candidate arenas invalidates it, so the later load is a
; use-after-free and the verifier rejects the program.
func main
    r5 = 0
    s0 = ARENA_CREATE DRAM_LOCAL, 64
    s1 = ARENA_CREATE DRAM_LOCAL, 64
    JGT r5, 0, other
    r6 = ARENA_ALLOC s0, [i64; 8]
    JMP join
other:
    r6 = ARENA_ALLOC s1, [i64; 8]
join:
    ARENA_DESTROY s0
    r7 = LOAD [r6]
    ARENA_DESTROY s1
    RET r7
endfunc
