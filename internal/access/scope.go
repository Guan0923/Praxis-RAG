package access

import "sort"

// The merge rule shared by every enforcement point:
//
//	allow := defaultAllow | libraryAllow | fileAllow | groupAllow
//	deny := defaultDeny | libraryDeny | fileDeny
//	effective := allow &^ deny
//	allowed := required != 0 && effective&required == required
//
// Any applicable deny wins over the same operation's allow, so a
// whole-library deny can override a file allow and a file deny can restrict a
// whole-library allow. Operations combine independently. Group grants only
// contribute allow bits: a group never reduces a member's existing
// permissions, so nothing from the group side lands on deny.

// EffectiveLibrary returns the effective operation mask at knowledge-base
// level: the role defaults merged with the whole-library rule and the
// whole-library group grant. File rules never contribute (the create bit is
// only decided here).
func (s ScopeSet) EffectiveLibrary() PermissionMask {
	allow := s.DefaultAllow | s.WholeAllow | s.GroupAllow[""]
	deny := s.DefaultDeny | s.WholeDeny
	return allow &^ deny
}

// EffectiveDocument returns the effective operation mask for one document:
// role defaults, the whole-library rule, the document's own rule and the
// document's group grant all merge, then every applicable deny is subtracted.
func (s ScopeSet) EffectiveDocument(name string) PermissionMask {
	allow := s.DefaultAllow | s.WholeAllow | s.GroupAllow[""] | s.FileAllow[name] | s.GroupAllow[name]
	deny := s.DefaultDeny | s.WholeDeny | s.FileDeny[name]
	return allow &^ deny
}

// EffectiveDocumentOrDefault is EffectiveDocument with a nil-map-safe
// fallback for zero-valued ScopeSet literals constructed outside
// NewScopeSet.
func (s ScopeSet) EffectiveDocumentOrDefault(name string) PermissionMask {
	if s.FileAllow == nil && s.FileDeny == nil && s.GroupAllow == nil {
		allow := s.DefaultAllow | s.WholeAllow
		deny := s.DefaultDeny | s.WholeDeny
		return allow &^ deny
	}
	return s.EffectiveDocument(name)
}

// allows reports whether the required operation bits survive the merge.
func allows(effective, required Operation) bool {
	return required != 0 && Operation(effective)&required == required
}

// CanCreate reports whether the account may add new documents to the
// knowledge base (library-level check only).
func (s ScopeSet) CanCreate() bool { return allows(Operation(s.EffectiveLibrary()), OpCreate) }

// CanReadDocument reports whether one document name is readable.
func (s ScopeSet) CanReadDocument(name string) bool {
	return allows(Operation(s.EffectiveDocumentOrDefault(name)), OpRead)
}

// CanUpdateDocument reports whether one existing document may be replaced.
func (s ScopeSet) CanUpdateDocument(name string) bool {
	return allows(Operation(s.EffectiveDocumentOrDefault(name)), OpUpdate)
}

// CanDeleteDocument reports whether one existing document may be deleted.
func (s ScopeSet) CanDeleteDocument(name string) bool {
	return allows(Operation(s.EffectiveDocumentOrDefault(name)), OpDelete)
}

// CanWriteDocument reports whether one document may be modified in any way
// (update or delete). It backs the legacy "write" wording.
func (s ScopeSet) CanWriteDocument(name string) bool {
	effective := Operation(s.EffectiveDocumentOrDefault(name))
	return allows(effective, OpUpdate) || allows(effective, OpDelete)
}

// AllowsAnyRead reports whether the account may read anything in the
// knowledge base: either the whole-library read survives the merge, or at
// least one file rule leaves its read bit effective.
func (s ScopeSet) AllowsAnyRead() bool {
	if Operation(s.EffectiveLibrary())&OpRead != 0 {
		return true
	}
	for name, allow := range s.FileAllow {
		if Operation(s.EffectiveDocumentOrDefault(name))&OpRead != 0 && Operation(allow)&OpRead != 0 {
			return true
		}
	}
	for name, allow := range s.GroupAllow {
		if name == "" {
			continue
		}
		if Operation(s.EffectiveDocumentOrDefault(name))&OpRead != 0 && Operation(allow)&OpRead != 0 {
			return true
		}
	}
	return false
}

// HasAnyWrite reports whether the account may modify anything in the
// knowledge base: any effective create, or any file rule that survives with
// update or delete.
func (s ScopeSet) HasAnyWrite() bool {
	if Operation(s.EffectiveLibrary())&(OpCreate|OpUpdate|OpDelete) != 0 {
		return true
	}
	for name := range s.FileAllow {
		if Operation(s.EffectiveDocumentOrDefault(name))&(OpUpdate|OpDelete) != 0 {
			return true
		}
	}
	for name := range s.GroupAllow {
		if name != "" && Operation(s.EffectiveDocumentOrDefault(name))&(OpUpdate|OpDelete) != 0 {
			return true
		}
	}
	return false
}

// ReadsWhole reports whether every document is readable without a
// restriction: the whole-library read bit survives and no file-level deny
// narrows it.
func (s ScopeSet) ReadsWhole() bool { return !s.Restricted() }

// WritesWhole reports whether every document is modifiable: the
// whole-library update or delete bit must survive the merge, and no
// file-level deny may narrow it.
func (s ScopeSet) WritesWhole() bool {
	if Operation(s.EffectiveLibrary())&(OpUpdate|OpDelete) == 0 {
		return false
	}
	for name, deny := range s.FileDeny {
		if Operation(deny)&(OpUpdate|OpDelete) != 0 &&
			Operation(s.EffectiveDocumentOrDefault(name))&(OpUpdate|OpDelete) == 0 {
			return false
		}
	}
	return true
}

// Restricted reports whether retrieval must be limited to an explicit
// document set: either the whole-library read does not survive the merge, or
// a file-level deny (which the whole-library allow cannot override) narrows
// the readable set.
func (s ScopeSet) Restricted() bool {
	if Operation(s.EffectiveLibrary())&OpRead == 0 {
		return true
	}
	for _, deny := range s.FileDeny {
		if Operation(deny)&OpRead != 0 {
			return true
		}
	}
	return false
}

// ReadDocumentNames returns the document names whose read bit is granted by
// a file rule and survives that rule's own deny, sorted. It is the legacy
// scope projection; deny-based restrictions are resolved through
// ResolveVisibleDocumentIDs instead.
func (s ScopeSet) ReadDocumentNames() []string {
	names := make([]string, 0, len(s.FileAllow))
	for name, allow := range s.FileAllow {
		if Operation(allow)&OpRead != 0 && Operation(s.EffectiveDocumentOrDefault(name))&OpRead != 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// WriteDocumentNames returns the document names whose update or delete bit
// is granted by a file rule and survives that rule's own deny, sorted.
func (s ScopeSet) WriteDocumentNames() []string {
	names := make([]string, 0, len(s.FileAllow))
	for name, allow := range s.FileAllow {
		if Operation(allow)&(OpUpdate|OpDelete) != 0 &&
			Operation(s.EffectiveDocumentOrDefault(name))&(OpUpdate|OpDelete) != 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ReadableDocumentNames returns every readable document name given the
// existing document names of the knowledge base: all names when unrestricted,
// otherwise the names that survive the merge (allow-list minus denies).
func (s ScopeSet) ReadableDocumentNames(existing []string) []string {
	if !s.Restricted() {
		return existing
	}
	readable := make([]string, 0, len(existing))
	for _, name := range existing {
		if s.CanReadDocument(name) {
			readable = append(readable, name)
		}
	}
	return readable
}
