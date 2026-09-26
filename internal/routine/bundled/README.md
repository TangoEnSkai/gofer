# Bundled routines

Routine specs in this directory (`<name>.yaml`) are embedded in the gofer
binary (`routine.Bundled`) and installed with
`gofer routine add <name> --from-bundled`. Other files are ignored.

Every bundled spec must pass `routine.Parse`; `TestBundledSpecsParse` checks it.
