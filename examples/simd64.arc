; simd64.arc - packed f64x2 and i64x2 arithmetic.
func main:
    v1 = f64x2(1.5, 2.5)
    v2 = f64x2(10.0, 20.0)
    v3 = VADD.f64x2 v1, v2
    PRINTV.f64x2 v3
    v4 = i64x2(100, 200)
    v5 = i64x2(1, 2)
    v6 = VADD.i64x2 v4, v5
    PRINTV.i64x2 v6
    RET
