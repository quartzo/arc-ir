; An extern declaration is a signature only: it must not have a body.
; The instruction after it would be silently attached to the declaration, so it
; is rejected.
extern func f(r1: i64) -> i64
    RET
func main -> i64
    RET
endfunc
