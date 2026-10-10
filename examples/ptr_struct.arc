; ptr_struct.arc - pass a struct by reference; the callee reads its fields.
struct Node { next: ptr(Node), val: scalar }

func main:
    r9 = ARENA_CREATE DRAM_LOCAL, 256
    r2 = ARENA_ALLOC r9, Node
    r3 = ARENA_ALLOC r9, Node
    r4 = 42
    r3.val = r4
    r2.next = r3
    r5 = 7
    r2.val = r5
    r1 = r2
    CALL read
    PRINT r0
    ARENA_DESTROY r9
    RET r0

func read(r1: ptr(Node)):
    r2 = r1.next
    r0 = r2.val
    RET r0
