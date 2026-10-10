; struct.arc - a named layout with a self-referential pointer field.
struct Node { next: ptr(Node), val: i64 }

func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC r1, Node
    r3 = ARENA_ALLOC r1, Node
    r4 = 42
    r3.val = r4             ; r3.val = 42
    r2.next = r3            ; r2.next = r3
    r5 = 7
    r2.val = r5             ; r2.val = 7
    r6 = r2.next            ; load the pointer field
    r7 = r6.val             ; r6's val = 42
    PRINT r7
    r8 = r2.val             ; load the scalar field
    PRINT r8
    ARENA_DESTROY r1
    RET r7
