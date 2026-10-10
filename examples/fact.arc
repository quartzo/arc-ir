; fact.arc - recursive factorial, exercising nested calls.
func main:
    r1 = 5
    CALL fact
    PRINT r0
    RET r0

func fact(r1: scalar):
    JGT r1, 1, rec
    r0 = 1
    RET r0
rec:
    r2 = SUB r1, 1
    r3 = r1
    r1 = r2
    CALL fact
    r4 = MUL r3, r0
    r0 = r4
    RET r0
