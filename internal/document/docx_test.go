package document

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildZipBytes builds an in-memory zip archive with the given members.
func buildZipBytes(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range members {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%q) error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip Close() error = %v", err)
	}
	return buffer.Bytes()
}

// patchFirstCentralEntry rewrites one 32-bit field of a zip image's first
// central-directory entry: offset 8 holds the general-purpose flags and
// offset 24 the uncompressed size.
func patchFirstCentralEntry(t *testing.T, raw []byte, offset int, value uint32) []byte {
	t.Helper()
	index := bytes.Index(raw, []byte{'P', 'K', 1, 2})
	if index < 0 {
		t.Fatalf("central directory entry not found in zip image")
	}
	binary.LittleEndian.PutUint32(raw[index+offset:], value)
	return raw
}

func writeDOCXImage(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.docx")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// TestValidateDOCXBoundedPackage pins the upload-protection branches of
// validateDOCX: a valid Word package passes, while too many members, an
// encrypted member and an oversized uncompressed total are each rejected
// with their frozen messages.
func TestValidateDOCXBoundedPackage(t *testing.T) {
	t.Run("valid package passes", func(t *testing.T) {
		path := writeDOCXImage(t, buildZipBytes(t, map[string]string{
			"[Content_Types].xml": "<Types/>",
			"word/document.xml":   "<w:document/>",
		}))
		if err := validateDOCX(path, "test.docx"); err != nil {
			t.Fatalf("validateDOCX(valid) error = %v", err)
		}
	})

	t.Run("too many members", func(t *testing.T) {
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		for index := 0; index <= MaxDOCXMembers; index++ {
			entry, err := writer.Create(fmt.Sprintf("entry-%d.xml", index))
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if _, err := entry.Write([]byte("x")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("zip Close() error = %v", err)
		}
		err := validateDOCX(writeDOCXImage(t, buffer.Bytes()), "test.docx")
		if err == nil || !strings.Contains(err.Error(), "Word 文件内部条目过多") {
			t.Fatalf("too-many-members error = %v, want the frozen message", err)
		}
	})

	t.Run("encrypted member", func(t *testing.T) {
		raw := patchFirstCentralEntry(t,
			buildZipBytes(t, map[string]string{"word/document.xml": "x"}), 8, 0x0001)
		err := validateDOCX(writeDOCXImage(t, raw), "test.docx")
		if err == nil || !strings.Contains(err.Error(), "Word 文件已加密，暂不支持需要密码的文件") {
			t.Fatalf("encrypted error = %v, want the frozen message", err)
		}
	})

	t.Run("oversized uncompressed total", func(t *testing.T) {
		raw := patchFirstCentralEntry(t,
			buildZipBytes(t, map[string]string{"word/document.xml": "x"}), 24, 0xFFFFFFF0)
		err := validateDOCX(writeDOCXImage(t, raw), "test.docx")
		if err == nil || !strings.Contains(err.Error(), "Word 解压后不能超过") {
			t.Fatalf("oversize error = %v, want the frozen message", err)
		}
	})
}
