package openwrt

// sectionIndex maps a record identity to the UCI sections holding it.
//
// A change set is applied against a single snapshot of the router, so the index
// is mutated as operations are performed. That keeps every decision — is this
// record present, is it mine, can I adopt it — a lookup rather than another
// round trip, and it makes duplicates inside one change set impossible.
type sectionIndex struct {
	byKey map[string][]indexedSection
}

type indexedSection struct {
	name  string
	owner string
}

func newSectionIndex(sections map[string]DNSRecord) *sectionIndex {
	index := &sectionIndex{byKey: make(map[string][]indexedSection, len(sections))}
	for name, record := range sections {
		key := record.Key()
		index.byKey[key] = append(index.byKey[key], indexedSection{name: name, owner: record.Owner})
	}
	return index
}

// owned returns the sections for key that this provider may modify. With
// ownership disabled every match qualifies.
func (i *sectionIndex) owned(key, ownershipID string, ownershipEnabled bool) []string {
	var sections []string
	for _, section := range i.byKey[key] {
		if ownershipEnabled && section.owner != ownershipID {
			continue
		}
		sections = append(sections, section.name)
	}
	return sections
}

// firstUnowned returns a section matching key that carries no ownership marker
// at all — a candidate for adoption. Sections owned by a different ID are not
// candidates: they belong to another instance.
func (i *sectionIndex) firstUnowned(key string) (string, bool) {
	for _, section := range i.byKey[key] {
		if section.owner == "" {
			return section.name, true
		}
	}
	return "", false
}

func (i *sectionIndex) add(key, section, ownershipID string) {
	i.byKey[key] = append(i.byKey[key], indexedSection{name: section, owner: ownershipID})
}

func (i *sectionIndex) markOwned(key, section, ownershipID string) {
	for pos, existing := range i.byKey[key] {
		if existing.name == section {
			i.byKey[key][pos].owner = ownershipID
			return
		}
	}
}

func (i *sectionIndex) drop(key, section string) {
	sections := i.byKey[key]
	for pos, existing := range sections {
		if existing.name == section {
			i.byKey[key] = append(sections[:pos:pos], sections[pos+1:]...)
			break
		}
	}
	if len(i.byKey[key]) == 0 {
		delete(i.byKey, key)
	}
}
