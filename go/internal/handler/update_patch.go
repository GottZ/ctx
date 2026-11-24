package handler

import "github.com/GottZ/ctx/internal/store"

// updatePatchEmpty reports whether an update's patch names no field: every
// field absent or null (a JSON null decodes to nil). Such a patch has nothing
// for the store's statement to set (issue #519: manage update answered it
// 500, "no fields to update"); an update item of a batch refuses it as
// missing_fields, and manage update does the same.
func updatePatchEmpty(d store.UpdateBlockData) bool {
	return d.Category == nil && d.Title == nil && d.Content == nil && d.Tags == nil &&
		d.Metadata == nil && d.MetadataMerge == nil && d.Edits == nil &&
		d.Scope == nil && d.Sensitivity == nil && d.Type == nil
}
