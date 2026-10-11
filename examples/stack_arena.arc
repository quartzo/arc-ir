; stack_arena.arc - a fixed arena backed by the current stack frame.
; The region lives in the frame, so it is never malloc'd and never freed; the
; allocation is a plain pointer bump with no runtime call.
func main -> i64
    r1 = ARENA_CREATE STACK, 128
    r2 = ARENA_ALLOC r1, [i64; 16]
    r3 = 0
loop:
    JGE r3, 10, done
    STORE [r2 + r3*8], r3
    r3 = ADD r3, 1
    JMP loop
done:
    r4 = LOAD [r2 + r3*8 - 8]
    PRINT r4
    RET r4
endfunc
