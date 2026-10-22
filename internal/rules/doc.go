// Package rules holds the decisions the laptop makes without the model, as
// pure functions over plain data: today the Slurm placement facts
// (SlurmHints). It must do no I/O of its own: no SSH, network or UI
// dependencies (TestNoHeavyImports checks the import graph): no
// internal/app, launch, sshx, gui, tui, backend, site or config.
package rules
