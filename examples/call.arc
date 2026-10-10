; call.arc - call a function that adds two numbers.
func main:
    r1 = 3
    r2 = 4
    CALL add
    PRINT r0
    RET r0

func add(r1: i64, r2: i64):
    r0 = ADD r1, r2
    RET r0
