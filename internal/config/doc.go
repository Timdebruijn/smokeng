// Package config will implement declarative TOML import/export of the target
// tree (DESIGN.md §7.3): path-keyed tables, unset keys mean "inherit" (NULL),
// import is an upsert-by-path sync that disables (never silently deletes)
// targets absent from the file, --prune deletes explicitly. Export writes
// only local values so export→import round-trips exactly. The SmokePing
// Targets importer (v0.2) also lives here.
//
// Status: not implemented; this is the next step after the scaffold, since it
// is the only way to get targets into the database before the admin UI.
package config
