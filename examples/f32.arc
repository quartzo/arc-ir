; f32.arc - 32-bit float arithmetic and conversions.
func main:
    v1 = f32(2.5)
    v2 = f32(4.0)
    v3 = FMUL32 v1, v2      ; 10.0
    v4 = FADD32 v3, v1      ; 12.5
    PRINTF32 v4
    r1 = F32TOI v4          ; 12
    PRINT r1
    v5 = ITOF32 r1          ; 12.0
    PRINTF32 v5
    RET r1
