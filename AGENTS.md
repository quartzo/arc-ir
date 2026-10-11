# AGENTS.md

Guidance for anyone (human or AI agent) working on this repository.

## What this is

ARC (Arena Register Code) is a flat-register assembly language with an
eBPF-style static verifier and an x86_64 backend. The product is the
**verifier** — the guarantee that a verified program has no undefined
behavior. Read [`docs/vision.md`](docs/vision.md) for the design and
[`docs/safety.md`](docs/safety.md) for the exact contract.

## The one rule: invariant-first

The differentiator is **soundness** of the verifier. Therefore:

1. Before adding a feature, state whether it **strengthens** or **preserves**
   the safety contract (`docs/safety.md`).
2. **Close gaps before adding features.** A known gap in the contract is more
   important than a new instruction.
3. A change that makes more programs verify but weakens the contract is a
   regression, not a feature.
4. When you cannot prove a property, the verifier must **reject**, not guess.
   Sound and incomplete beats complete and unsound.

If a change is ambiguous or consequential, **lay out the alternatives first**
(in the conversation or a doc) before writing code.

## Layout

```
cmd/arcc/        CLI (verify / emit-asm / build)
pkg/ir/          flat-register IR, memory operands, functions, CFG
pkg/parser/      .arc text syntax (text/scanner)
pkg/verifier/    abstract interpretation: types, ranges, arenas, ownership, memory
pkg/codegen/     x86_64 GNU assembly backend (register allocation + spilling)
pkg/runtime/     embedded C runtime (arena bump allocator, print)
docs/            assembly reference, vision, safety contract
examples/        valid and invalid .arc programs
```

## Conventions

- Go, `gofmt`-clean, `go vet`-clean, `go test ./...` green.
- All documentation and comments in **English**.
- Every verifier rule gets a test; every rejection gets an `examples/invalid_*.arc`
  when it is illustrative.
- Prefer small, focused commits; the commit message says how the contract is
  affected.

## Adding an instruction

1. Add the opcode to `pkg/ir` (and `HasDest` / `IsTerminator` as needed).
2. Parse it in `pkg/parser`.
3. Verify it in `pkg/verifier` — this is the important part: what does it do to
   each register's abstract state? Which invariant does it preserve?
4. Emit it in `pkg/codegen`.
5. Add tests (parser + verifier + compile-and-run where useful).
6. Document it in `docs/assembly.md`.

## Testing the contract

- Unit tests cover each rejection in `pkg/verifier`.
- `FuzzVerify` checks the verifier never panics on arbitrary input.
- `pkg/codegen` compiles and runs programs and checks their output.
- The strongest future test is a checker that runs accepted programs under a
  bounds-checked interpreter and asserts no violation — not yet implemented.

## Current status

See the table in [`README.md`](README.md). In short: flat IR, eBPF-style
verifier (ranges, typed memory, function-scoped regions, cumulative capacity),
parser, x86_64 backend, C runtime, CLI, docs.
