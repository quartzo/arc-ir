; invalid_cross_region.arc - a reference may not be stored across regions.
; The callee gets two borrowed regions (r1, r2) and tries to link a node in r1
; to a node in r2. The two parameter regions are unordered, so one may be freed
; before the other; the store is rejected. Copy the data into one region first
; (see examples/deep_copy.arc).
struct Node
    next: ptr(Node)
    val: i64
endstruct

func main -> i64
    s0 = ARENA_CREATE DRAM_LOCAL, 256
    r1 = s0
    r2 = s0
    CALL link
    RET
endfunc

func link(r1: arena, r2: arena) -> i64
    r3 = ARENA_ALLOC r1, Node
    r4 = ARENA_ALLOC r2, Node
    r3.next = r4
    RET
endfunc
