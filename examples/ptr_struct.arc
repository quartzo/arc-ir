; ptr_struct.arc - pass a struct by reference; the callee reads its fields.
; The arena handle survives the call, so it lives in a preserved register (s0).

struct Node
    next: ptr(Node)
    val: i64
endstruct

func main -> i64
    s0 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC s0, Node
    r3 = ARENA_ALLOC s0, Node
    r4 = 42
    r3.val = r4
    r2.next = r3
    r5 = 7
    r2.val = r5
    r1 = r2
    CALL read
    PRINT r0
    RET r0
endfunc

func read(r1: ptr(Node)) -> i64
    r2 = r1.next
    r0 = r2.val
    RET r0
endfunc
