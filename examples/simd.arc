; simd.arc - packed f32x4 and i32x4 arithmetic.
func main -> i64
    v1 = f32x4(1.0, 2.0, 3.0, 4.0)
    v2 = f32x4(10.0, 20.0, 30.0, 40.0)
    v3 = VADD v1, v2
    PRINTV v3
    v4 = i32x4(1, 2, 3, 4)
    v5 = i32x4(10, 20, 30, 40)
    v6 = VADD v4, v5
    PRINTV v6
    RET
endfunc
