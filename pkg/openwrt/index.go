package openwrt

// sectionIndex maps a record identity to the UCI sections holding it.
//
// A change set is applied against a single snapshot of the router, so the index
// is mutated as operations are performed. That keeps every decision — is this
// record present, is it mine, can I adopt it — a lookup rather than another
// round trip, and it makes duplicates inside one change set impossible.
type sectionIndex map[string][]indexedSection

type indexedSection struct {
	name string
	// record is stored canonical, so named compares names the way Key does.
	record DNSRecord
}

func newSectionIndex(sections map[string]DNSRecord) sectionIndex {
	index := make(sectionIndex, len(sections))
	for name, record := range sections {
		index.add(record, name, record.Owner)
	}
	return index
}

// owned returns the sections for key that this provider may modify. With
// ownership disabled (an empty ID) every match qualifies.
func (i sectionIndex) owned(key, ownershipID string) []string {
	var sections []string
	for _, section := range i[key] {
		if ownershipID != "" && section.record.Owner != ownershipID {
			continue
		}
		sections = append(sections, section.name)
	}
	return sections
}

// firstUnowned returns a section matching key that carries no ownership marker
// at all — a candidate for adoption. Sections owned by a different ID are not
// candidates: they belong to another instance.
func (i sectionIndex) firstUnowned(key string) (string, bool) {
	for _, section := range i[key] {
		if section.record.Owner == "" {
			return section.name, true
		}
	}
	return "", false
}

// named returns every section holding a record of this type and name, whoever
// owns it and whatever its value.
func (i sectionIndex) named(recordType, name string) []indexedSection {
	name = CanonicalName(name)
	var matches []indexedSection
	for _, sections := range i {
		for _, section := range sections {
			if section.record.Type == recordType && section.record.Name == name {
				matches = append(matches, section)
			}
		}
	}
	return matches
}

// closesCNAMELoop reports whether a CNAME from alias to target would let the
// indexed CNAMEs, whoever owns them, lead from target back to alias.
func (i sectionIndex) closesCNAMELoop(alias, target string) bool {
	alias = CanonicalName(alias)
	seen := map[string]bool{}
	for next := []string{CanonicalName(target)}; len(next) > 0; {
		name := next[len(next)-1]
		next = next[:len(next)-1]
		if name == alias {
			return true
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, section := range i.named(RecordTypeCNAME, name) {
			next = append(next, section.record.Value)
		}
	}
	return false
}

func (i sectionIndex) add(record DNSRecord, section, ownershipID string) {
	record = record.canonical()
	record.Owner = ownershipID
	key := record.Key()
	i[key] = append(i[key], indexedSection{name: section, record: record})
}

func (i sectionIndex) markOwned(key, section, ownershipID string) {
	for pos, existing := range i[key] {
		if existing.name == section {
			i[key][pos].record.Owner = ownershipID
			return
		}
	}
}

func (i sectionIndex) drop(key, section string) {
	sections := i[key]
	for pos, existing := range sections {
		if existing.name == section {
			i[key] = append(sections[:pos:pos], sections[pos+1:]...)
			break
		}
	}
	if len(i[key]) == 0 {
		delete(i, key)
	}
}
