; invalid_stack_arg.arc - a stack arena cannot be passed to a function.
; Its backing store is the caller's frame and it is never freed, so the callee
; would treat it as a heap arena. Pass a pointer into it instead.
func main -> i64
    r1 = ARENA_CREATE STACK, 64
    CALL fill
    RET
endfunc

func fill(r1: arena) -> i64
    RET
endfunc
