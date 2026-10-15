module github.com/jm2/switchyard

go 1.27

toolchain go1.27.2

// Report Windows junctions and mount points as symlinks, so the symlink
// checks that confine agent-spawned sessions to a directory (and the agy
// workspace checks) also see through them. Since Go 1.23 they would
// otherwise be followed silently.
godebug winsymlink=0
