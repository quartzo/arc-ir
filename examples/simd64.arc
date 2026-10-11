; simd64.arc - packed f64x2 and i64x2 arithmetic.
func main
    v1 = f64x2(1.5, 2.5)
    v2 = f64x2(10.0, 20.0)
    v3 = VADD v1, v2
    PRINTV v3
    v4 = i64x2(100, 200)
    v5 = i64x2(1, 2)
    v6 = VADD v4, v5
    PRINTV v6
    RET
endfunc
