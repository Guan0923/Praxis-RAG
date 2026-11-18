package document

// cloneStrings returns a defensive copy of a string slice, normalizing nil to
// an empty slice. It was shared with the removed in-process OOXML reader and
// stays the copy primitive for Word/PDF asset records and manifests.
func cloneStrings(values []string) []string {
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}
