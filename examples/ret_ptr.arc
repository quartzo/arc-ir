; ret_ptr.arc - a function returns an arena, another returns a data pointer.
; The arena handle survives the second call, so it lives in s0.
func main:
    CALL get_arena          ; r0 = arena
    s0 = r0                 ; keep the arena handle (preserved)
    r1 = s0                 ; arg to make
    CALL make               ; make(r1) -> ptr([i64; 8])
    r2 = r0
    r3 = 99
    STORE [r2], r3
    r4 = LOAD [r2]
    PRINT r4
    ARENA_DESTROY s0
    RET r4

func get_arena() -> arena:
    r0 = ARENA_CREATE DRAM_LOCAL, 64
    RET r0

func make(r1: arena) -> ptr([i64; 8]):
    r0 = ARENA_ALLOC r1, [i64; 8]
    RET r0
