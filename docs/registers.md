# ARC registers

The general-purpose register file has two blocks:

| block     | names         | semantics                |
|-----------|---------------|--------------------------|
| scratch   | `r0`..`r31`   | volatile across a call   |
| preserved | `s0`..`s31`   | callee-saved             |

The vector file is unchanged (`v0`..`v15`, the x64 line).

## Scratch

Scratch is volatile: **any call destroys the whole scratch block**. Using a
scratch register after a call is an error ("use of destroyed register"). A value
that must survive a call lives in a preserved register (`s*`).

## Preserved

Preserved is callee-saved: a call leaves it unchanged. The backend saves, in
its frame, the preserved registers a function actually uses; the verifier does
not report them destroyed.

## Physical mapping

The 32+32 logical file maps onto the target as:

| block     | physical | in the thread area |
|-----------|----------|--------------------|
| scratch   | 5        | 27                 |
| preserved | 10       | 22                 |

The area is a fixed per-thread region, addressed through a reserved base
register. A preserved register whose home would be clobbered by a call is saved
on the stack if the function actually uses it.

## Status

The logical blocks and their semantics are implemented: scratch is volatile and
preserved is callee-saved, with the backend saving the preserved registers a
function uses and no `saveAll` around calls. The physical/area split above is
**not yet implemented** — the backend still uses its earlier mapping (physical
registers plus per-function stack slots).
