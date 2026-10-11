; enum.arc - a tagged union with tag refinement.
; A value is constructed with VARIANT; the callee refines the active variant
; with JV and can then read that variant's fields.
; The arena handle survives the call, so it lives in s0.
enum Value
    Int(n: i64)
    Pair(a: i32, b: i32)
    Nil
endenum

func main
    s0 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC s0, Value
    VARIANT r2, Value.Int
    r3 = 21
    r2.n = r3               ; r2 is a Value.Int holding 21
    r1 = r2
    CALL twice
    PRINT r0
    RET
endfunc

func twice(r1: ptr(Value)) -> i64
    JV r1, Value.Int, as_int
    JV r1, Value.Pair, as_pair
    r0 = 0
    RET r0

as_int:
    r0 = r1.n
    r0 = ADD r0, r0
    RET r0

as_pair:
    r2 = r1.a
    r3 = r1.b
    r0 = ADD r2, r3
    RET r0
endfunc
