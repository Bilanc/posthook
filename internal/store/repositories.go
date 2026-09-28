package store

// RepositoryRoots returns the root path of every repository posthook has
// recorded, oldest first. Paths are as they were at ingest time and may no
// longer exist.
func (db *DB) RepositoryRoots() ([]string, error) {
	rows, err := db.Query(`SELECT root_path FROM repositories ORDER BY created_at, root_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}
