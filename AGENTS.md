## Testing & Verification Guidelines

### 1. Ephemeral Verification vs. Persistent Tests
- **Verify Without Polluting:** You are encouraged to verify your work, but NEVER pollute the `tests/` directory with one-off verification scripts (e.g., checking if a file was deleted, testing if an API call works, or verifying a simple refactor).
- **Run and Burn:** For temporary self-checks, use CLI one-liners (e.g., `python -c "..."`, `node -e "..."`) or create temporary scripts (`_temp_verify.*` / `/tmp/*`) and **DELETE them immediately** after verification passes.
- **Persistence Gate:** Only commit tests to the repository if they protect core domain logic against future regressions.

### 2. High-ROI Unit Testing Standards (Anti-Bloat)
- **Test Behavior, Not Implementation:** Treat the system under test as a black box. Assert ONLY on public API outputs and final observable state changes. NEVER test private methods or replicate internal implementation steps.
- **Target High-Value Logic Only:** Write persistent unit tests ONLY for complex algorithms, business rules, state machines, edge cases, and bug regressions.
- **Explicit Skip List:** DO NOT write unit tests for trivial code, including getters/setters, DTOs/models, configuration loading, framework boilerplate, or simple CRUD pass-throughs without branching logic.
- **Table-Driven First:** Consolidate happy paths, boundary conditions, and error cases for the same function into a single parameterized/table-driven test. Avoid repetitive `test_xxx_1`, `test_xxx_2` functions.
- **Minimal Mocking:** Mock ONLY out-of-process boundaries (external APIs, databases, filesystem, time, randomness). Do NOT mock internal collaborators or assert on internal call counts (`verify` / `assert_called_once`) unless the side effect itself is the core contract.

## Commenting & Documentation Guidelines (Signal Over Noise)

- **Explain WHY, Never WHAT:** Code must be self-explanatory through clear naming and structure. Write comments ONLY to explain *why* a non-obvious decision was made (trade-offs, historical pitfalls, upstream bugs, or counter-intuitive business rules). NEVER write comments that merely narrate or translate what the code is doing line-by-line.
- **No Conversational Leftovers:** NEVER leave meta-comments referencing the current prompt, task, or refactoring process (e.g., `// Updated as requested`, `// Fixed bug here`, `// Added new logic`). Write comments as if the code has always existed in its final state.
- **Restrained Docstrings:** Add formal docstrings (`/** ... */` or `"""..."""`) ONLY to complex public APIs or exported module interfaces. Skip boilerplate `@param`/`@return` blocks on internal helpers, simple functions, or self-evident type signatures.
- **Keep It Concise:** A comment should never be longer than the code it describes unless documenting a critical architectural constraint or complex algorithm (e.g., complex Regex, bitwise math, lock-free concurrency).