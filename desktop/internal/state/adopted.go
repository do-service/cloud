package state

// SetAdopted records the sync folder's top-level names found when it was
// connected (migration 0003), replacing what was there.
func (t *Tx) SetAdopted(names []string) error {
	if _, err := t.exec(`DELETE FROM adopted`); err != nil {
		return err
	}
	for _, n := range names {
		if _, err := t.exec(`INSERT INTO adopted (rel_path) VALUES (?) ON CONFLICT DO NOTHING`, n); err != nil {
			return err
		}
	}
	return nil
}

// Adopted lists what SetAdopted recorded.
func (t *Tx) Adopted() ([]string, error) {
	rows, err := t.query(`SELECT rel_path FROM adopted ORDER BY rel_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
