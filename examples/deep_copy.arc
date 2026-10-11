; deep_copy.arc - copy data from a source region into a destination region.
; The return names the destination (r1), so the result lives entirely in dst
; even though the input lives in src. Crossing regions requires a copy; storing
; the src pointer directly into dst memory would be rejected.
struct Point
    x: i64
    y: i64
endstruct

func main -> i64
    r5 = ARENA_CREATE DRAM_LOCAL, 256   ; src region
    r6 = ARENA_CREATE DRAM_LOCAL, 256   ; dst region

    r1 = r5
    r2 = ARENA_ALLOC r5, Point
    r3 = 3
    r2.x = r3
    r4 = 4
    r2.y = r4

    r1 = r6                             ; dst arena (the return region)
    CALL copy_point                     ; r2 still holds the src pointer

    r3 = r0.x
    PRINT r3
    r4 = r0.y
    PRINT r4
    RET
endfunc

; Allocate in dst, copy the fields out of the src region, and return.
func copy_point(r1: arena, r2: ptr(Point)) -> r1: ptr(Point)
    r0 = ARENA_ALLOC r1, Point
    r3 = r2.x
    r0.x = r3
    r4 = r2.y
    r0.y = r4
    RET r0
endfunc
