---
name: swift-property-wrapper-keypath-debugging
description: Diagnose and fix Swift property wrapper keypath errors (e.g., @InjectedObservable, @Injected) using grep-based codebase scanning and surgical corrections.
triggers:
  - swift compiler error mentioning "generic constraint" or "type mismatch" with property wrapper expecting a keypath
  - error on @InjectedObservable, @Injected, or similar DI property wrappers
  - user mentions "keypath" + "@Injected" compile failure
difficulty: intermediate
expected_time: 5-15 min
platforms: [ios, macos, swift]
---

# Swift Property Wrapper Keypath Debugging

Property wrappers like `@InjectedObservable`, `@Injected`, or custom DI wrappers often require a **keypath** (`\Type.property`) to locate the dependency in a container. Passing the property reference directly (`Container.property`) passes the *value* not the keypath, causing generic constraint failures.

## Pattern

**Wrong:** `@InjectedObservable(Container.billingManager)`
**Right:** `@InjectedObservable(\Container.billingManager)`

The backslash turns the property access into a `KeyPath` literal.

## Process

1. **Read the compiler error.** Look for:
   - "generic constraint" failures
   - "type mismatch" with `Observable`/`Factory`/container types
   - mentions of "keyPath" or "WritableKeyPath"

2. **Grep for the wrapper usage.**
   ```bash
   grep -rn '@InjectedObservable' path/to/project --include='*.swift'
   # or for multiple wrappers
   grep -rnE '@Injected(Observable|Property|Lazy)' path --include='*.swift'
   ```

3. **Inspect each match.** Identify those missing the `\` keypath prefix.
   - Correct: `@InjectedObservable(\Container.billingManager)`
   - Incorrect: `@InjectedObservable(Container.billingManager)`

4. **Fix only the broken ones.** Use `patch` or targeted edits. Do NOT refactor adjacent code.

5. **Verify.** Re-run grep to confirm zero remaining incorrect patterns:
   ```bash
   grep -rn '@InjectedObservable(Container\.' path --include='*.swift'
   ```

6. **Build.** Ensure compiler error resolved.

## Pitfalls

- **Don't confuse with other `@Injected` variants:** Some wrappers take the factory directly (no keypath). Only fix if error explicitly says keypath expected.
- **Escaping in grep:** Use `\.` to match literal dot; use `Container\.` to avoid partial matches.
- **Module qualification:** If using `ModuleName.Container.property`, keypath must include module: `\ModuleName.Container.property`.
- **Multiple containers:** Some projects have `Container.main`, `Container.test` — adjust accordingly.
- **Don't preemptively fix correct code:** Only change lines that the compiler error actually implicates.

## When to Use

- Swift compile error: "type 'Factory<X>' does not conform to protocol 'Observable'"
- "generic parameter 'Value' could not be inferred"
- "key path literal refers to instance member 'x' of type 'T'"
- Any DI property wrapper that should receive a keypath but receives a value instead.

## Verification

After fix:
- `grep` for wrong pattern returns nothing.
- Xcode/SwiftCLI build succeeds.
- App runs; injected dependencies resolve at runtime.
