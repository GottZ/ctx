package migrations

// A shipped migration is frozen by its checksum: the runner records
// sha256(file) in _migrations when it applies a version, and the schema
// contract (internal/schemacontract) reports any later difference between
// that record and the embedded file as a breaking migration_integrity drift.
//
// Amending a shipped migration is the exception, reserved for a migration
// that cannot complete on some databases. A database that already applied the
// earlier revision skips the version and keeps that revision's checksum, so
// the contract has to know it. amended lists those earlier revisions per
// version; every entry says which releases shipped it and why the file
// changed. The current file's checksum never belongs here.
var amended = map[int][]string{
	// 156_guard_tenant_candidates.sql as shipped in 5.29.1–5.29.3. Its
	// data repair un-archived rows without checking the partial unique
	// indexes over NOT is_archived, so a database with an archived row beside
	// a live twin hit a 23505, rolled the migration back and could not boot.
	156: {"c095c3918345c0fa7c78e96bc9172a2a38aa790baa3b155f67514edba40ca121"},
}

// IsAmendedChecksum reports whether checksum is the recorded checksum of an
// earlier shipped revision of migration version (see amended).
func IsAmendedChecksum(version int, checksum string) bool {
	for _, c := range amended[version] {
		if c == checksum {
			return true
		}
	}
	return false
}
