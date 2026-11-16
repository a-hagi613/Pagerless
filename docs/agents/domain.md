# Domain Documentation

How agents and contributors should use this repository's domain documentation. The repository has a single context.

## Read before exploring

- [`GLOSSARY.md`](../../GLOSSARY.md) at the repository root.
- [`docs/adr/`](../adr/README.md): read the records that touch the area you are about to change.

## Use the glossary's vocabulary

When output names a domain concept (an issue title, a proposal, a test name, an identifier), use the term exactly as defined in `GLOSSARY.md`, and avoid the synonyms it lists under _Avoid_.

If a needed concept is not in the glossary, either the language is being invented (reconsider it) or there is a genuine gap, which should be resolved and added through the `domain-modeling` skill.

## Respect the ADRs

If a change contradicts an existing ADR, state this explicitly rather than overriding it silently, for example:

> _Contradicts ADR-0005 (two Go binaries), but worth revisiting because..._

A decision that is reversed gets a new ADR, and the original is marked `Superseded`.
