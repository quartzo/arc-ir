; float.arc - f64 scalar arithmetic and integer<->float conversions.
func main
    v1 = 2.5
    v2 = 4.0
    v3 = FMUL v1, v2        ; 10.0
    v4 = FADD v3, v1        ; 12.5
    PRINTF v4
    r1 = FTOI v4            ; 12
    PRINT r1
    v5 = ITOF r1            ; 12.0
    PRINTF v5
    RET r1
endfunc
