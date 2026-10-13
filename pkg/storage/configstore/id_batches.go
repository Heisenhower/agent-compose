package configstore

const sqliteIDQueryBatchSize = 500

func forEachIDBatch(ids []string, fn func([]string) error) error {
	for start := 0; start < len(ids); start += sqliteIDQueryBatchSize {
		end := min(start+sqliteIDQueryBatchSize, len(ids))
		if err := fn(ids[start:end]); err != nil {
			return err
		}
	}
	return nil
}
