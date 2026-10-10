; invalid_capacity.arc - each allocation fits, but their sum exceeds the arena.
; The verifier tracks cumulative allocation, so this is rejected statically
; (the runtime would otherwise abort).
func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 128
    r2 = ARENA_ALLOC r1, 64
    r3 = ARENA_ALLOC r1, 64
    r4 = ARENA_ALLOC r1, 64
    ARENA_DESTROY r1
    RET
