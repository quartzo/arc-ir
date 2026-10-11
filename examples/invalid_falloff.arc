; A function may not fall off its end. Here make declares a pointer return
; but reaches the end without RET, so the caller would receive 0 typed as a
; live pointer. Rejected: "function make may fall off its end".
func main -> i64
    CALL make
    r1 = LOAD [r0]
    PRINT r1
    RET
endfunc

func make -> ptr([i64; 8])
    r1 = 1
endfunc
