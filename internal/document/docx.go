package document

import (
	"archive/zip"
	"io"
	"path"
	"strings"
)

// docxArchive is the zip package of one Word document with lazily parsed XML
// parts, mirroring WordReader's cache of lxml trees.
type docxArchive struct {
	reader *zip.ReadCloser
	files  map[string]*zip.File
	parts  map[string]*xmlNode
}

// openDOCX opens (but does not validate) a Word package.
func openDOCX(path string) (*docxArchive, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	archive := &docxArchive{
		reader: reader,
		files:  make(map[string]*zip.File, len(reader.File)),
		parts:  make(map[string]*xmlNode, 8),
	}
	for _, file := range reader.File {
		archive.files[file.Name] = file
	}
	return archive, nil
}

func (a *docxArchive) Close() error {
	if a == nil || a.reader == nil {
		return nil
	}
	return a.reader.Close()
}

// Has reports whether a part exists, like `name in archive.namelist()`.
func (a *docxArchive) Has(name string) bool {
	_, ok := a.files[name]
	return ok
}

// missingWordPartError marks a package part that a parsed XML part references
// but the zip does not contain (Python's KeyError from archive.read).
type missingWordPartError struct{ name string }

func (e *missingWordPartError) Error() string { return "missing Word part: " + e.name }

// readPart reads one part with a hard decompression cap so a lying zip header
// cannot expand past the validated package budget.
func (a *docxArchive) readPart(name string) ([]byte, error) {
	file, ok := a.files[name]
	if !ok {
		return nil, &missingWordPartError{name: name}
	}
	handle, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	limit := MaxDOCXUncompressedBytes + 1
	if limit < 1 {
		limit = 1
	}
	data, err := io.ReadAll(io.LimitReader(handle, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// part parses and caches one XML part, mirroring WordReader.part.
func (a *docxArchive) part(name string) (*xmlNode, error) {
	if cached, ok := a.parts[name]; ok {
		return cached, nil
	}
	data, err := a.readPart(name)
	if err != nil {
		return nil, err
	}
	node, err := parseXMLNode(data)
	if err != nil {
		return nil, err
	}
	a.parts[name] = node
	return node, nil
}

// validateDOCX mirrors document_readers._validate_docx_archive: member count,
// zip encryption flag and combined uncompressed size are bounded before any
// part is parsed.
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

// relation is one entry of a Word relationship part.
type relation struct {
	target   string
	external bool
	kind     string
}

// rels returns the resolved relationships of one part, mirroring
// WordReader.rels and posixpath.normpath/posixpath.join resolution.
func (r *wordReader) rels(partName string) (map[string]relation, error) {
	if cached, ok := r.relations[partName]; ok {
		return cached, nil
	}
	folder, base := posixSplit(partName)
	relsPath := folder + "/_rels/" + base + ".rels"
	relations := make(map[string]relation)
	if r.archive.Has(relsPath) {
		node, err := r.archive.part(relsPath)
		if err != nil {
			return nil, err
		}
		for _, item := range node.Children {
			if item.Name.Local != "Relationship" {
				continue
			}
			target := item.attrValue("", "Target", "")
			resolved := ""
			if target != "" {
				if strings.HasPrefix(target, "/") {
					resolved = strings.TrimPrefix(target, "/")
				} else {
					resolved = posixClean(path.Join(folder, target))
				}
			}
			relations[item.attrValue("", "Id", "")] = relation{
				target:   resolved,
				external: item.attrValue("", "TargetMode", "") == "External",
				kind:     item.attrValue("", "Type", ""),
			}
		}
	}
	r.relations[partName] = relations
	return relations, nil
}

// posixSplit mirrors posixpath.split: returns the directory and file name.
func posixSplit(name string) (string, string) {
	index := strings.LastIndex(name, "/")
	if index < 0 {
		return "", name
	}
	return name[:index], name[index+1:]
}

// posixClean mirrors posixpath.normpath closely enough for OOXML part targets.
func posixClean(name string) string {
	return path.Clean(name)
}

// contentTypes holds the parsed [Content_Types].xml maps.
type contentTypes struct {
	// byExtension maps lower-case extension (without dot) to content type.
	byExtension map[string]string
	// overrides maps part name (without leading slash) to content type.
	overrides map[string]string
}

// loadContentTypes mirrors WordReader.__init__'s [Content_Types].xml parsing.
func loadContentTypes(archive *docxArchive) (contentTypes, error) {
	node, err := archive.part("[Content_Types].xml")
	if err != nil {
		return contentTypes{}, err
	}
	result := contentTypes{
		byExtension: make(map[string]string),
		overrides:   make(map[string]string),
	}
	for _, item := range node.Children {
		if extension := item.attrValue("", "Extension", ""); extension != "" {
			result.byExtension[strings.ToLower(extension)] = item.attrValue("", "ContentType", "")
		}
		if partName := item.attrValue("", "PartName", ""); partName != "" {
			result.overrides[strings.TrimPrefix(partName, "/")] = item.attrValue("", "ContentType", "")
		}
	}
	return result, nil
}
