; invalid_vec_call.arc - the vector file is volatile: a call destroys every
; vector register, so using one afterwards is rejected.
func main:
    v1 = 2.5
    CALL clobber
    PRINTF v1
    RET

func clobber:
    v1 = 1.0
    RET
