; typed_mem.arc - a struct with fields of different widths.
; The verifier tracks each field's type and width, so a store to a u8 field
; writes one byte and a load zero-extends the result.
struct Rec
    a: u8
    b: u16
    c: u32
    d: u64
endstruct

func main -> i64
    r1 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC r1, Rec
    r3 = 200
    r2.a = r3               ; one byte
    r4 = 40000
    r2.b = r4               ; two bytes
    r5 = 3000000000
    r2.c = r5               ; four bytes
    r6 = 123456789
    r2.d = r6               ; eight bytes
    r7 = r2.a
    PRINT r7
    r8 = r2.b
    PRINT r8
    r9 = r2.c
    PRINT r9
    r10 = r2.d
    PRINT r10
    RET r7
endfunc
