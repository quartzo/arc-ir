# ARC registers

The general-purpose register file has two blocks with different semantics:

| block     | names         | semantics                |
|-----------|---------------|--------------------------|
| scratch   | `r0`..`r31`   | volatile across a call   |
| preserved | `s0`..`s31`   | callee-saved             |

The vector file is unchanged (`v0`..`v15`, the x64 line).

## Scratch

A scratch register is volatile: a call may destroy it. The verifier marks as
**destroyed** the scratch registers a callee writes — detected from the callee's
body, transitively through the calls it makes — and rejects using one
afterwards ("use of destroyed register").

## Preserved

A preserved register is callee-saved: a call leaves it unchanged. The backend
detects the preserved registers a function writes and saves/restores them in
the prologue/epilogue. A value that must survive a call lives in a preserved
register (`s*`).

## Detection, not declaration

Nothing is annotated. The compiler derives, per function, the registers it
writes: scratch writes become a caller error, preserved writes are saved.

## Open decisions

- **Physical mapping.** The 32+32 logical file is mapped onto the target. The
  plan is 5 scratch + 10 preserved physical registers, the rest in a fixed
  per-thread memory area addressed through a reserved base register. Today the
  backend still uses its earlier mapping (physical registers + per-function
  stack slots).
- **Scratch precision.** Today a scratch register is destroyed only if the
  callee actually writes it. The stricter reading of "volatile" is that *any*
  scratch register must be considered destroyed by any call.
