/*
 * arc_runtime.c - C support runtime for ARC assembly programs.
 *
 * The generated x86_64 code calls these four functions. Arenas are bump
 * allocators over a single calloc'd backing block; the memory tier is recorded
 * but, for the reference runtime, all tiers share the same backing store.
 *
 * The functions use the System V AMD64 ABI: first integer argument in %rdi,
 * second in %rsi, return value in %rax. The verifier proves that no program
 * reaches these functions with out-of-bounds or destroyed arenas; the checks
 * here are a defence-in-depth backstop.
 */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

typedef struct {
    uint8_t *base;
    int64_t  size;
    int64_t  used;
    int64_t  tier;
    int32_t  destroyed;
} ArcArena;

long arc_arena_create(long tier, long size) {
    ArcArena *a = (ArcArena *)malloc(sizeof(ArcArena));
    if (!a) {
        abort();
    }
    a->base = (uint8_t *)calloc(1, (size_t)size);
    if (!a->base) {
        abort();
    }
    a->size = size;
    a->used = 0;
    a->tier = tier;
    a->destroyed = 0;
    return (long)a;
}

long arc_arena_alloc(long arena, long size) {
    ArcArena *a = (ArcArena *)arena;
    if (!a || a->destroyed) {
        abort();
    }
    if (a->used + size > a->size) {
        abort();
    }
    long p = (long)(a->base + a->used);
    a->used += size;
    return p;
}

void arc_arena_destroy(long arena) {
    ArcArena *a = (ArcArena *)arena;
    if (!a) {
        return;
    }
    free(a->base);
    a->destroyed = 1;
    free(a);
}

void arc_print(long v) {
    printf("%lld\n", (long long)v);
}
