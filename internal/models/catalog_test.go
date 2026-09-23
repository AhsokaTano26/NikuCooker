package models

import "testing"

func TestCatalogCarriesChineseSelectionGuidance(t *testing.T) {
	for _, entry := range Catalog {
		if entry.Note == "" || entry.Recommendation == "" || entry.Accuracy == "" ||
			entry.Speed == "" || entry.Hardware == "" || entry.Language == "" || len(entry.Tags) == 0 {
			t.Errorf("model %s has incomplete selection guidance: %#v", entry.Name, entry)
		}
	}

	medium, ok := Lookup(KindASR, "medium")
	if !ok || !containsTag(medium.Tags, "日常推荐") {
		t.Errorf("medium must be marked as the everyday recommendation: %#v", medium)
	}
	distil, ok := Lookup(KindASR, "distil-large-v3")
	if !ok || !containsTag(distil.Tags, "日语不推荐") {
		t.Errorf("distil-large-v3 must disclose its Japanese limitation: %#v", distil)
	}
}

func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
