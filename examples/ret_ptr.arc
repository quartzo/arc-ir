; ret_ptr.arc - a function returns an arena, another returns a data pointer.
func main:
    CALL get_arena          ; r0 = arena
    r1 = r0                 ; keep the arena handle
    CALL make               ; make(r1) -> ptr(64)
    r2 = r0
    r3 = 99
    STORE [r2], r3
    r4 = LOAD [r2]
    PRINT r4
    ARENA_DESTROY r1
    RET r4

func get_arena() -> arena:
    r0 = ARENA_CREATE DRAM_LOCAL, 64
    RET r0

func make(r1: arena) -> ptr(64):
    r0 = ARENA_ALLOC r1, 64
    RET r0
