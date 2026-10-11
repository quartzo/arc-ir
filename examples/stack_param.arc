; stack_param.arc - borrow a stack arena.
; The caller creates the region in its frame; the callee allocates from it.
; The callee's allocation is an inline bump on the caller's frame cursor.
func fill(r1: stack(64)) -> r1: ptr([i64; 8])
    r0 = ARENA_ALLOC r1, [i64; 8]
    r2 = 42
    STORE [r0], r2
    RET r0
endfunc

func main -> i64
    s0 = ARENA_CREATE STACK, 64
    r1 = s0
    CALL fill
    r2 = LOAD [r0]
    PRINT r2
    RET r2
endfunc
