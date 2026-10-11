# ARC safety contract

This document states exactly what `arcc verify` guarantees, and what it does
not. It is the project's core promise; everything else (the ISA, the backends,
the syntax) exists to serve it.

## The guarantee

If `arcc verify` accepts a program, then running the emitted x86_64 code
cannot cause undefined behavior:

- no out-of-bounds load or store,
- no use of a pointer into a released arena (use-after-free),
- no double release of an arena,
- no use of an uninitialized register,
- no allocation that exceeds an arena's declared capacity (cumulative, across
  the whole program, including through function calls),
- no type misuse (loading through an integer, storing an arena handle in memory,
  letting a region escape the function that creates it, ...).

The guarantee is **whole-program**: it holds across function calls, because
functions are verified against typed signatures and every call site is checked
against them.

Arena capacity comes in two modes. A **fixed** arena has a capacity the
verifier proves is never exceeded, so it never fails at run time. A
**growable** arena adds chunks on demand and may abort on out-of-memory — a
safe failure (like Rust's default allocator), not undefined behavior. Either
way, no allocation overflows memory.

A **stack** arena is a fixed arena whose backing store is the creating
function's frame; it is never `malloc`'d, never freed, and cannot outlive the
function by construction. Its capacity is proven exactly like a fixed arena's,
and allocating from it is an inline pointer bump with no runtime call. A callee
may allocate from a borrowed stack arena only through a `stack(size)` parameter,
whose byte budget the caller must have free and accounts for when the call
returns, so the capacity is still proven across calls.

## Sound, not complete

The verifier is **sound**: it never accepts a program that violates the
guarantee. It is **not complete**: it may reject a program that is in fact
safe. A rejection means "cannot prove", not "unsafe". Soundness is the
contract; completeness is best-effort.

## Foreign functions (FFI)

An `extern` declaration (see [`assembly.md`](assembly.md) §6.1) calls a function
that is not verified — its body is foreign C code. The verifier checks the call
site against the declared signature and applies that signature's effect (scratch
and vector registers are destroyed, the result gets the declared type, pointed-to
cells are invalidated), but it **cannot** see the callee's body.

Therefore the guarantee above holds for a program that uses `extern` only under
an added assumption:

> every foreign function obeys its declared signature, does not retain a
> pointer past the return of the call, and reads and writes only within the
> regions it is passed.

This is the same trust already placed in the runtime, but at an open boundary
rather than a fixed one. A program with no `extern` declaration is unaffected:
for it the guarantee is unconditional. A foreign function that violates the
assumption can cause undefined behaviour the verifier will not catch.

A pointer returned by a foreign function has unknown provenance and extent. It
is tracked as an **opaque token**: ARC may not load or store through it, but may
pass it back to another foreign function.

## What is NOT guaranteed

- **Termination.** The verifier proves memory safety, not that the program
  halts. Loops and recursion may run forever.
- **No leaks.** A region is released automatically at the `RET` of the function
  that creates it, so a live region never leaks. A leak can still occur if the
  handle is overwritten before `RET`, which the analysis may not see. Leaks are
  safe, not undefined behavior — Rust makes the same allowance for
  `mem::forget`.
- **Bounded stack.** Deep recursion may overflow the machine stack; the
  verifier does not bound call depth (unlike the Linux eBPF verifier).

## The analysis, in one paragraph

The verifier is an abstract interpreter over the control-flow graph. Per
program point it tracks, for each register, an abstract value: an integer (with
a signed range and width), arena handle (with reference identity and capacity),
or data pointer (with arena, allocation identity and size). It also
tracks the type of statically-addressed memory cells and the bytes allocated so
far from each arena. States flow along edges and merge at joins (a conservative
union). Conditional jumps refine integer ranges on each edge. A join that would
merge two different arena handles is rejected rather than approximated: the
value would name one of several arenas, whose capacity cannot be accounted. A
region is function-scoped: it is released when the function that creates it
returns, and it cannot escape that function. A pointer return names the
parameter whose region it lives in, and a reference may be stored only into a
region that outlives it (a region created here only into memory that also dies
here; two distinct parameter regions are unordered and cannot be linked).

## Non-goals / known threats

- **Bugs in the verifier itself.** The range arithmetic and the merge are
  hand-written and not formally proven. `FuzzVerify` checks robustness (no
  panics on arbitrary input), not soundness.
- **The runtime.** The C support runtime is trusted. The verifier proves the
  runtime's checks are never needed; the checks remain as a backstop.
- **Concurrency.** ARC is single-threaded; there are no data races to reason
  about yet.

## How to change the verifier safely

Any change to the verifier must state whether it **strengthens** or
**preserves** the guarantee above. A change that weakens it is a regression,
even if it makes more programs verify. New features come after gaps are closed,
not before.
