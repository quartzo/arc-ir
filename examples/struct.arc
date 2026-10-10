; struct.arc - a named layout with a pointer field and a scalar field.
struct Node { next: ptr(8), val: scalar }

func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC r1, Node
    r3 = ARENA_ALLOC r1, 8
    r4 = 42
    STORE [r3], r4          ; *r3 = 42
    r2.next = r3            ; Node.next = r3
    r5 = 7
    r2.val = r5             ; Node.val = 7
    r6 = r2.next            ; load the pointer field
    r7 = LOAD [r6]          ; *r6 = 42
    PRINT r7
    r8 = r2.val             ; load the scalar field
    PRINT r8
    ARENA_DESTROY r1
    RET r7
