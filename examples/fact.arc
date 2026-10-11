; fact.arc - recursive factorial, exercising nested calls.
; A value kept across a call lives in a preserved register (s0); scratch
; registers (r*) are destroyed by a call.
func main
    r1 = 5
    CALL fact
    PRINT r0
    RET r0
endfunc

func fact(r1: i64)
    JGT r1, 1, rec
    r0 = 1
    RET r0
rec:
    r2 = SUB r1, 1
    s0 = r1
    r1 = r2
    CALL fact
    r0 = MUL s0, r0
    RET r0
endfunc
