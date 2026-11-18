package access

import "fmt"

// The document operation bits. Every rule stores two masks (allow and deny);
// each operation is independently unset, allowed or denied on a rule.
type Operation uint16

const (
	// OpRead (0001) may read one document (preview, original file,
	// attachments, retrieval evidence).
	OpRead Operation = 1 << iota
	// OpCreate (0010) may add new documents to a knowledge base. It only
	// applies at library level: file-scoped rules reject the bit.
	OpCreate
	// OpUpdate (0100) may replace the content of one existing document.
	OpUpdate
	// OpDelete (1000) may remove one existing document.
	OpDelete
)

// MaskAll is every operation bit (1111 / 15).
const MaskAll Operation = OpRead | OpCreate | OpUpdate | OpDelete

// PermissionMask is the numeric form used by REST and the state layer.
type PermissionMask = uint16

// String renders the bit set as the documented 4-digit binary form.
func (o Operation) String() string {
	return fmt.Sprintf("%04b", uint16(o))
}

// ParseMask validates one caller-supplied mask value. Unknown bits and
// negative values are rejected by the write interfaces.
func ParseMask(value int64) (PermissionMask, error) {
	if value < 0 || value > int64(MaskAll) {
		return 0, NewError(KindValidation, MessageMaskBad)
	}
	return PermissionMask(value), nil
}

// MaskAllowsFileScope reports whether a mask may be stored on a file-scoped
// rule: the create bit belongs to the knowledge-base level only.
func MaskAllowsFileScope(mask PermissionMask) bool {
	return Operation(mask)&OpCreate == 0
}

// LegacyMask converts the historical permission level into its allow mask:
// read grants the read bit, a whole-library write grants every operation and
// a file-scoped write grants read+update+delete (the create bit is library
// level). Denies stay zero: historical rules never denied.
func LegacyMask(permission string, wholeLibrary bool) PermissionMask {
	switch permission {
	case statePermissionWrite:
		if wholeLibrary {
			return uint16(MaskAll)
		}
		return uint16(OpRead | OpUpdate | OpDelete)
	case statePermissionRead:
		return uint16(OpRead)
	default:
		return 0
	}
}

// LegacyPermission is the conservative historical projection of one allow
// mask: "write" only when read+update+delete are all granted, "read" when at
// least the read bit is granted, and "" for anything else (so a legacy reader
// never sees more access than the mask actually grants).
func LegacyPermission(allow PermissionMask) string {
	if Operation(allow)&(OpRead|OpUpdate|OpDelete) == (OpRead | OpUpdate | OpDelete) {
		return statePermissionWrite
	}
	if Operation(allow)&OpRead != 0 {
		return statePermissionRead
	}
	return ""
}

// statePermission read/write literals mirror the state layer constants
// without exporting a dependency cycle in this file's helpers.
const (
	statePermissionRead  = "read"
	statePermissionWrite = "write"
)

// OperationLabel renders one operation bit for logs and error messages.
func OperationLabel(op Operation) string {
	switch op {
	case OpRead:
		return "阅读"
	case OpCreate:
		return "新增"
	case OpUpdate:
		return "修改"
	case OpDelete:
		return "删除"
	default:
		return "未知"
	}
}
