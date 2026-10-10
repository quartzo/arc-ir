; f32.arc - 32-bit float arithmetic and conversions.
func main:
    v1 = f32(2.5)
    v2 = f32(4.0)
    v3 = FMUL v1, v2      ; 10.0
    v4 = FADD v3, v1      ; 12.5
    PRINTF v4
    r1 = FTOI v4          ; 12
    PRINT r1
    v5 = ITOF32 r1        ; 12.0
    PRINTF v5
    RET r1
