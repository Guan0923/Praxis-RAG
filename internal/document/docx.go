package document

import (
	"archive/zip"
)

// validateDOCX mirrors document_readers._validate_docx_archive: member count,
// zip encryption flag and combined uncompressed size are bounded before the
// package is handed to the Docling service (or any other consumer). It no
// longer feeds an in-process OOXML parser: Word parsing happens only in the
// Docling HTTP service, and this check protects the local upload path.
func validateDOCX(path, name string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return invalidDocumentErrf("Word 文件损坏或格式无效：%s", name)
	}
	defer reader.Close()
	members := reader.File
	if len(members) > MaxDOCXMembers {
		return invalidDocumentErrf("Word 文件内部条目过多：%s", name)
	}
	for _, member := range members {
		if member.Flags&0x1 != 0 {
			return invalidDocumentErrf("Word 文件已加密，暂不支持需要密码的文件：%s", name)
		}
	}
	uncompressed := int64(0)
	for _, member := range members {
		if int64(member.UncompressedSize64) > MaxDOCXUncompressedBytes-uncompressed {
			uncompressed = MaxDOCXUncompressedBytes + 1
			break
		}
		uncompressed += int64(member.UncompressedSize64)
	}
	if uncompressed > MaxDOCXUncompressedBytes {
		limitMiB := MaxDOCXUncompressedBytes / (1024 * 1024)
		return invalidDocumentErrf("Word 解压后不能超过 %d MiB：%s", limitMiB, name)
	}
	return nil
}
