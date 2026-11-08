package document

import (
	"encoding/xml"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// headingStyle mirrors re.fullmatch(r"heading\s*([1-9])", name, re.I).
var headingStyle = regexp.MustCompile(`(?i)^heading\s*([1-9])$`)

var nulPlaceholder = regexp.MustCompile(`\x00[^\x00]+\x00`)

// referenceInstruction mirrors the REF field scan in word_reader.read_paragraph:
// `re.findall(r"\bREF\s+([^\s\\]+)", ...)`.
var referenceInstruction = regexp.MustCompile(`\bREF\s+([^\s\\]+)`)

// styleInfo is one entry of word/styles.xml.
type styleInfo struct {
	name    string
	basedOn string
}

// cellRef identifies the parent cell of a nested Word table.
type cellRef struct {
	assetID string
	row     int
	col     int
}

// wordReader ports rag_mcp.word_reader.WordReader: ordered OOXML extraction with
// explicit picture/table assets, caption/note association and fail-closed
// handling for structures that cannot be converted reliably.
//
// Formula conversion is bounded in process (see formula.go); deployments that
// need full docxlatex coverage can inject a FormulaConverter via ParseOptions.
type wordReader struct {
	archive      *docxArchive
	contentTypes contentTypes
	blocks       []WordManifestBlock
	assets       []*WordManifestAsset
	images       map[string][]byte
	headings     []string
	paragraph    int
	section      int
	characters   int
	serial       int
	skipped      int
	styles       map[string]styleInfo
	relations    map[string]map[string]relation
	visitedNotes map[string]struct{}
	diagnostics  []WordDiagnostic
	formula      FormulaConverter
}

// ParseOptions configures optional external integrations while parsing.
type ParseOptions struct {
	// FormulaConverter overrides the bounded in-process OMML converter.
	FormulaConverter FormulaConverter
}

// ParseWordPackage extracts a Word package into a storage-decoupled manifest
// plus image bytes keyed by manifest asset id. It validates the archive bounds
// (entry count, encryption flag, uncompressed size) first, like
// word_reader.parse_docx.
func ParseWordPackage(filePath string, options ...ParseOptions) (*WordPackage, error) {
	if err := validateDOCX(filePath, baseName(filePath)); err != nil {
		return nil, err
	}
	pkg, err := parseWordPackage(filePath, options...)
	if err != nil {
		if _, ok := AsReadError(err); ok {
			return nil, err
		}
		// word_reader.parse_docx wraps missing parts and malformed XML as
		// "Word document is invalid or incomplete"; keep that ingestion-level
		// message for the runtime while intentional ValueError equivalents
		// (unsupported structures, merged-cell errors, ...) pass through.
		var syntaxError *xml.SyntaxError
		var missingPart *missingWordPartError
		if errors.As(err, &syntaxError) || errors.As(err, &missingPart) {
			return nil, fmt.Errorf("Word document is invalid or incomplete: %s: %v", baseName(filePath), err)
		}
		return nil, err
	}
	return pkg, nil
}

func parseWordPackage(filePath string, options ...ParseOptions) (*WordPackage, error) {
	archive, err := openDOCX(filePath)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	reader := &wordReader{
		archive:      archive,
		images:       make(map[string][]byte),
		styles:       make(map[string]styleInfo),
		relations:    make(map[string]map[string]relation),
		visitedNotes: make(map[string]struct{}),
		diagnostics:  make([]WordDiagnostic, 0),
		formula:      BoundedFormulaConverter{},
	}
	if len(options) > 0 && options[0].FormulaConverter != nil {
		reader.formula = options[0].FormulaConverter
	}
	if archive.Has("word/styles.xml") {
		stylesNode, err := archive.part("word/styles.xml")
		if err != nil {
			return nil, err
		}
		for _, style := range stylesNode.childrenWhere(nsWord, "style") {
			id, _ := style.wordAttr("styleId")
			info := styleInfo{}
			if nameNode := style.child(nsWord, "name"); nameNode != nil {
				info.name, _ = nameNode.wordAttr("val")
			}
			if basedNode := style.child(nsWord, "basedOn"); basedNode != nil {
				info.basedOn, _ = basedNode.wordAttr("val")
			}
			reader.styles[id] = info
		}
	}
	types, err := loadContentTypes(archive)
	if err != nil {
		return nil, err
	}
	reader.contentTypes = types
	if err := reader.read(); err != nil {
		return nil, err
	}
	return &WordPackage{Manifest: reader.manifest(), Images: reader.images}, nil
}

func (r *wordReader) manifest() WordManifest {
	blocks := make([]WordManifestBlock, len(r.blocks))
	copy(blocks, r.blocks)
	assets := make([]*WordManifestAsset, len(r.assets))
	copy(assets, r.assets)
	return WordManifest{
		Version:            WordParserVersion,
		Blocks:             blocks,
		Assets:             assets,
		Diagnostics:        r.diagnostics,
		SkippedObjectCount: r.skipped,
	}
}

func (r *wordReader) read() error {
	document, err := r.archive.part("word/document.xml")
	if err != nil {
		return err
	}
	body := document.child(nsWord, "body")
	if body == nil {
		return fmt.Errorf("Word document has no body")
	}
	if err := r.walk(body, "word/document.xml", "body"); err != nil {
		return err
	}
	references := make([]*xmlNode, 0, 4)
	body.walkDescendants(func(node *xmlNode) bool {
		if isElement(node, nsWord, "headerReference") || isElement(node, nsWord, "footerReference") {
			references = append(references, node)
		}
		return true
	})
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		rid := relationshipID(reference)
		relations, err := r.rels("word/document.xml")
		if err != nil {
			return err
		}
		target := ""
		external := false
		if relation, ok := relations[rid]; ok {
			target, external = relation.target, relation.external
		}
		if external || target == "" {
			return fmt.Errorf("Invalid Word header/footer reference")
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		r.headings = nil
		partNode, err := r.archive.part(target)
		if err != nil {
			return err
		}
		if err := r.walk(partNode, target, posixBase(target)); err != nil {
			return err
		}
	}
	for index := range r.blocks {
		r.blocks[index].Order = index
	}
	skipped, err := r.associate()
	if err != nil {
		return err
	}
	r.skipped = skipped
	for index := range r.blocks {
		r.blocks[index].Order = index
	}
	return nil
}

func relationshipID(node *xmlNode) string {
	if value, ok := node.attrNS(nsRelationships, "id"); ok {
		return value
	}
	if value, ok := node.attrNS("", "id"); ok {
		return value
	}
	return ""
}

func posixBase(name string) string {
	_, base := posixSplit(name)
	return base
}

// walk ports WordReader.walk: paragraphs and tables are emitted in order,
// wrappers are descended into, deleted content is ignored and embedded content
// that cannot be converted is rejected.
func (r *wordReader) walk(parent *xmlNode, part, scope string) error {
	for _, node := range parent.Children {
		switch {
		case isElement(node, nsWord, "p"):
			if _, err := r.readParagraph(node, part, scope, true); err != nil {
				return err
			}
		case isElement(node, nsWord, "tbl"):
			if _, err := r.readTable(node, part, scope, nil); err != nil {
				return err
			}
		case isElement(node, nsWord, "del"), isElement(node, nsWord, "moveFrom"),
			isElement(node, nsWord, "sdtPr"), isElement(node, nsWord, "sectPr"):
			continue
		case isElement(node, nsMarkupCompat, "AlternateContent"):
			selected := node.child(nsMarkupCompat, "Choice")
			if selected == nil {
				selected = node.child(nsMarkupCompat, "Fallback")
			}
			if selected == nil {
				return fmt.Errorf("Word AlternateContent has no Choice or Fallback in %s", part)
			}
			if err := r.walk(selected, part, scope); err != nil {
				return err
			}
		case isElement(node, nsWord, "altChunk"), isElement(node, nsWord, "subDoc"):
			return fmt.Errorf("Unsupported embedded Word content in %s: %s", part, node.Name.Local)
		default:
			if err := r.walk(node, part, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// style ports WordReader.style: the chain of style ids and names from the
// paragraph style through basedOn.
func (r *wordReader) style(paragraph *xmlNode) []string {
	value := ""
	if styleNode := paragraph.findPath(wordName("pPr"), wordName("pStyle")); styleNode != nil {
		value, _ = styleNode.wordAttr("val")
	}
	names := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	for value != "" {
		if _, ok := seen[value]; ok {
			break
		}
		seen[value] = struct{}{}
		info, ok := r.styles[value]
		if !ok {
			info = styleInfo{name: value}
		}
		names = append(names, value, info.name)
		value = info.basedOn
	}
	return names
}

func (r *wordReader) location(part, scope string) WordLocation {
	return WordLocation{
		Part:      part,
		Paragraph: r.paragraph,
		Headings:  cloneStrings(r.headings),
		Scope:     scope,
		Section:   r.section,
	}
}

// blockExtra carries the optional manifest fields of one emitted block.
type blockExtra struct {
	assetID    string
	bookmarks  []string
	references []string
	inCell     bool
}

// appendBlock ports WordReader.block: bounded append with a stable serial ref.
func (r *wordReader) appendBlock(kind, text string, location WordLocation, extra blockExtra) error {
	r.characters += utf8.RuneCountInString(text)
	if r.characters > MaxExtractedCharacters {
		return fmt.Errorf("Word extracted text exceeds the size limit")
	}
	r.serial++
	r.blocks = append(r.blocks, WordManifestBlock{
		Ref:        "block-" + strconv.Itoa(r.serial),
		Kind:       kind,
		Text:       text,
		Location:   location,
		AssetID:    extra.assetID,
		InCell:     extra.inCell,
		Bookmarks:  cloneStrings(extra.bookmarks),
		References: cloneStrings(extra.references),
	})
	return nil
}

// readParagraph ports WordReader.read_paragraph, including the NUL-marker
// protocol used to splice textbox/note/picture blocks back into paragraph order.
func (r *wordReader) readParagraph(node *xmlNode, part, scope string, emit bool) (string, error) {
	r.paragraph++
	headings := cloneStrings(r.headings)
	styles := r.style(node)
	headingLevel := 0
	for _, name := range styles {
		if match := headingStyle.FindStringSubmatch(name); match != nil {
			headingLevel, _ = strconv.Atoi(match[1])
			break
		}
	}
	offset := len(r.blocks)
	raw, err := r.inline(node, part, scope)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(nulPlaceholder.ReplaceAllString(raw, ""))
	if headingLevel > 0 && scope == "body" {
		if headingLevel-1 < len(r.headings) {
			r.headings = cloneStrings(r.headings[:headingLevel-1])
		} else {
			r.headings = cloneStrings(r.headings)
		}
		r.headings = append(r.headings, text)
		r.section++
		headings = cloneStrings(r.headings)
	}
	location := WordLocation{
		Part:      part,
		Paragraph: r.paragraph,
		Headings:  headings,
		Scope:     scope,
		Section:   r.section,
	}
	if emit {
		events := make(map[string]WordManifestBlock, len(r.blocks)-offset)
		for _, block := range r.blocks[offset:] {
			events[block.Ref] = block
		}
		r.blocks = r.blocks[:offset]
		kind := KindParagraph
		switch {
		case headingLevel > 0:
			kind = KindHeading
		case parseCaption(text) != nil:
			kind = KindCaption
		case styleMentionsCaption(styles):
			kind = KindCaption
		case noteLine.MatchString(text):
			kind = KindNote
		}
		bookmarks := bookmarkNames(node)
		references := paragraphReferences(node)
		for _, segment := range splitNULSegments(raw) {
			if strings.HasPrefix(segment, "\x00") {
				if event, ok := events[strings.Trim(segment, "\x00")]; ok {
					r.blocks = append(r.blocks, event)
				}
				continue
			}
			if trimmed := strings.TrimSpace(segment); trimmed != "" {
				if err := r.appendBlock(kind, trimmed, location, blockExtra{
					bookmarks: bookmarks, references: references,
				}); err != nil {
					return "", err
				}
			}
		}
	} else if text != "" {
		switch {
		case parseCaption(text) != nil:
			if err := r.appendBlock(KindCaption, text, location, blockExtra{inCell: true}); err != nil {
				return "", err
			}
		case noteLine.MatchString(text):
			if err := r.appendBlock(KindNote, text, location, blockExtra{inCell: true}); err != nil {
				return "", err
			}
		}
	}
	if !emit {
		r.characters += utf8.RuneCountInString(text)
		if r.characters > MaxExtractedCharacters {
			return "", fmt.Errorf("Word extracted text exceeds the size limit")
		}
	}
	return text, nil
}

// bookmarkNames mirrors `[el.get(w:name) for el in node.iter(w:bookmarkStart)]`.
func bookmarkNames(node *xmlNode) []string {
	names := make([]string, 0, 2)
	node.walkDescendants(func(candidate *xmlNode) bool {
		if isElement(candidate, nsWord, "bookmarkStart") {
			value, _ := candidate.wordAttr("name")
			names = append(names, value)
		}
		return true
	})
	return names
}

// paragraphReferences mirrors the REF field xpath in word_reader.read_paragraph.
func paragraphReferences(node *xmlNode) []string {
	parts := make([]string, 0, 2)
	node.walkDescendants(func(candidate *xmlNode) bool {
		if isElement(candidate, nsWord, "instrText") {
			parts = append(parts, candidate.Text)
		}
		if isElement(candidate, nsWord, "fldSimple") {
			if value, ok := candidate.wordAttr("instr"); ok {
				parts = append(parts, value)
			}
		}
		return true
	})
	if len(parts) == 0 {
		return nil
	}
	matches := referenceInstruction.FindAllStringSubmatch(strings.Join(parts, " "), -1)
	references := make([]string, 0, len(matches))
	for _, match := range matches {
		references = append(references, match[1])
	}
	return references
}

// inline ports WordReader.inline.
func (r *wordReader) inline(node *xmlNode, part, scope string) (string, error) {
	switch {
	case isElement(node, nsWord, "del"), isElement(node, nsWord, "moveFrom"),
		isElement(node, nsWord, "instrText"), isElement(node, nsWord, "pPr"),
		isElement(node, nsWord, "rPr"), isElement(node, nsWord, "sdtPr"):
		return "", nil
	case isElement(node, nsWord, "t"):
		return node.Text, nil
	case isElement(node, nsWord, "tab"), isElement(node, nsWord, "ptab"):
		return "\t", nil
	case isElement(node, nsWord, "br"), isElement(node, nsWord, "cr"):
		return "\n", nil
	case isElement(node, nsWord, "noBreakHyphen"):
		return "\u2011", nil
	case isElement(node, nsWord, "softHyphen"):
		return "\u00ad", nil
	case isElement(node, nsWord, "sym"):
		return "", fmt.Errorf("Unsupported legacy symbol font at %s, paragraph %d", part, r.paragraph)
	}
	if isElement(node, nsDrawingML, "graphicData") &&
		!node.hasDescendant(nsDrawingML, "blip") && !node.hasDescendant(nsWord, "txbxContent") {
		return "", fmt.Errorf("Unsupported embedded chart or diagram at %s, paragraph %d", part, r.paragraph)
	}
	if isElement(node, nsMath, "oMath") {
		oMathXML, err := serializeOMML(node)
		if err != nil {
			return "", fmt.Errorf("Word formula conversion failed: %v", err)
		}
		latex, err := r.formula.ConvertOMML(oMathXML)
		if err != nil {
			return "", err
		}
		return "$" + latex + "$", nil
	}
	if isElement(node, nsWord, "object") {
		return "", fmt.Errorf("Unsupported embedded OLE object at %s, paragraph %d", part, r.paragraph)
	}
	if isElement(node, nsDrawingML, "blip") || isElement(node, nsVML, "imagedata") {
		reference, err := r.picture(node, part, scope)
		if err != nil {
			return "", err
		}
		return "\x00" + reference + "\x00", nil
	}
	if isElement(node, nsWord, "txbxContent") {
		offset := len(r.blocks)
		if err := r.walk(node, part, scope+"/textbox-"+strconv.Itoa(r.paragraph)); err != nil {
			return "", err
		}
		var builder strings.Builder
		for _, block := range r.blocks[offset:] {
			builder.WriteString("\x00" + block.Ref + "\x00")
		}
		return builder.String(), nil
	}
	if isElement(node, nsWord, "footnoteReference") || isElement(node, nsWord, "endnoteReference") {
		return r.noteReference(node, part, scope)
	}
	if isElement(node, nsMarkupCompat, "AlternateContent") {
		selected := node.child(nsMarkupCompat, "Choice")
		if selected == nil {
			selected = node.child(nsMarkupCompat, "Fallback")
		}
		if selected == nil {
			return "", fmt.Errorf("Word AlternateContent has no Choice or Fallback at %s, paragraph %d", part, r.paragraph)
		}
		return r.inline(selected, part, scope)
	}
	var builder strings.Builder
	for _, child := range node.Children {
		value, err := r.inline(child, part, scope)
		if err != nil {
			return "", err
		}
		builder.WriteString(value)
	}
	return builder.String(), nil
}

// noteReference ports the footnote/endnote branch of WordReader.inline.
func (r *wordReader) noteReference(node *xmlNode, part, scope string) (string, error) {
	noteKind := "footnote"
	if isElement(node, nsWord, "endnoteReference") {
		noteKind = "endnote"
	}
	noteID, _ := node.wordAttr("id")
	key := noteKind + "\x00" + noteID
	if _, visited := r.visitedNotes[key]; visited {
		return " [" + noteKind + " " + noteID + "]", nil
	}
	offset := len(r.blocks)
	r.visitedNotes[key] = struct{}{}
	notePart := "word/" + noteKind + "s.xml"
	notes, err := r.archive.part(notePart)
	if err != nil {
		return "", fmt.Errorf("Missing %s %s", noteKind, noteID)
	}
	var matching *xmlNode
	for _, child := range notes.childrenWhere(nsWord, noteKind) {
		if id, _ := child.wordAttr("id"); id == noteID {
			matching = child
			break
		}
	}
	if matching == nil {
		return "", fmt.Errorf("Missing %s %s", noteKind, noteID)
	}
	if err := r.walk(matching, notePart, noteKind+"-"+noteID); err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString(" [" + noteKind + " " + noteID + "]")
	for _, block := range r.blocks[offset:] {
		builder.WriteString("\x00" + block.Ref + "\x00")
	}
	return builder.String(), nil
}

// picture ports WordReader.picture: validate the relationship/part/content type,
// keep the image bytes in the package image map and register a picture asset.
func (r *wordReader) picture(node *xmlNode, part, scope string) (string, error) {
	rid := ""
	for _, name := range []string{"embed", "id", "link"} {
		if value, ok := node.attrNS(nsRelationships, name); ok && value != "" {
			rid = value
			break
		}
	}
	relations, err := r.rels(part)
	if err != nil {
		return "", err
	}
	target := ""
	external := false
	if relation, ok := relations[rid]; ok {
		target, external = relation.target, relation.external
	}
	if external || target == "" || strings.HasPrefix(target, "../") || !r.archive.Has(target) {
		return "", fmt.Errorf("Missing or external Word image at %s, paragraph %d", part, r.paragraph)
	}
	mime := r.contentTypes.overrides[target]
	if mime == "" {
		mime = r.contentTypes.byExtension[strings.ToLower(strings.TrimPrefix(path.Ext(target), "."))]
	}
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("Unsupported image content type: %s", target)
	}
	data, err := r.archive.readPart(target)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("Empty Word image: %s", target)
	}
	assetID := "picture-" + strconv.Itoa(len(r.assets)+1)
	imageFile := "images/" + assetID + path.Ext(target)
	location := r.location(part, scope)
	labels := make([]string, 0, 3)
	if container := node.ancestorWhere(func(candidate *xmlNode) bool {
		return isElement(candidate, nsWord, "drawing") || isElement(candidate, nsWord, "pict")
	}); container != nil {
		container.walkDescendants(func(candidate *xmlNode) bool {
			if !isElement(candidate, nsWordDrawing, "docPr") {
				return true
			}
			for _, key := range []string{"name", "title", "descr"} {
				value, _ := candidate.attrNS("", key)
				labels = append(labels, value)
			}
			return true
		})
	}
	bookmarks := []string{}
	if paragraph := node.ancestorWhere(func(candidate *xmlNode) bool {
		return isElement(candidate, nsWord, "p")
	}); paragraph != nil {
		bookmarks = bookmarkNames(paragraph)
	}
	mimeCopy := mime
	imageCopy := imageFile
	r.assets = append(r.assets, &WordManifestAsset{
		AssetID:       assetID,
		Kind:          "picture",
		ImageFile:     &imageCopy,
		MIMEType:      &mimeCopy,
		Caption:       "",
		CaptionSource: "none",
		Captions:      make([]WordAnnotation, 0),
		Footnotes:     make([]WordAnnotation, 0),
		Location:      location,
		Labels:        labels,
		Bookmarks:     bookmarks,
	})
	r.images[assetID] = data
	if err := r.appendBlock(KindPicture, "", location, blockExtra{assetID: assetID}); err != nil {
		return "", err
	}
	return r.blocks[len(r.blocks)-1].Ref, nil
}

// readTable ports WordReader.read_table: gridBefore/gridSpan/gridAfter,
// vertical merges with anchor cells, nested tables, header rows and the
// rejection of legacy horizontal merges and invalid grids.
func (r *wordReader) readTable(node *xmlNode, part, scope string, parentCell *cellRef) (string, error) {
	assetID := "table-" + strconv.Itoa(len(r.assets)+1)
	location := r.location(part, scope)
	table := &WordTableData{
		Rows:       make([][]string, 0),
		Cells:      make([]WordTableCellData, 0),
		Footnotes:  make([]string, 0),
		HeaderRows: make([][]string, 0),
		ParentCell: nil,
	}
	if parentCell != nil {
		table.ParentCell = &WordParentCell{AssetID: parentCell.assetID, Row: parentCell.row, Col: parentCell.col}
	}
	r.assets = append(r.assets, &WordManifestAsset{
		AssetID:       assetID,
		Kind:          "table",
		ImageFile:     nil,
		MIMEType:      nil,
		Caption:       "",
		CaptionSource: "none",
		Captions:      make([]WordAnnotation, 0),
		Footnotes:     make([]WordAnnotation, 0),
		Table:         table,
		Location:      location,
		Labels:        make([]string, 0),
		Bookmarks:     bookmarkNames(node),
	})
	if err := r.appendBlock("table", "", location, blockExtra{assetID: assetID}); err != nil {
		return "", err
	}
	assetIndex := len(r.assets) - 1
	columns := 0
	if grid := node.child(nsWord, "tblGrid"); grid != nil {
		columns = len(grid.Children)
	}
	active := make(map[int]int)
	wrappers := []xmlName{
		wordName("sdt"), wordName("sdtContent"), wordName("ins"), wordName("moveTo"), wordName("customXml"),
	}
	for rowNumber, row := range descendantsNamed(node, nsWord, "tr", wrappers) {
		column := 0
		if before := row.findPath(wordName("trPr"), wordName("gridBefore")); before != nil {
			value, err := wordInt(before, "val", 0)
			if err != nil {
				return "", err
			}
			column = value
		}
		values := make([]string, column)
		nextActive := make(map[int]int)
		for _, cell := range descendantsNamed(row, nsWord, "tc", wrappers) {
			span := 1
			if spanNode := cell.findPath(wordName("tcPr"), wordName("gridSpan")); spanNode != nil {
				value, err := wordInt(spanNode, "val", 1)
				if err != nil {
					return "", err
				}
				span = value
			}
			if span < 1 || column+span > 1000 {
				return "", fmt.Errorf("Invalid Word table grid: %s", assetID)
			}
			vmerge := cell.findPath(wordName("tcPr"), wordName("vMerge"))
			continuation := false
			if vmerge != nil {
				value, _ := vmerge.wordAttr("val")
				continuation = value == "" || value == "continue"
			}
			if cell.findPath(wordName("tcPr"), wordName("hMerge")) != nil {
				return "", fmt.Errorf("Unsupported legacy horizontal table merge: %s", assetID)
			}
			cellText := make([]string, 0, 2)
			nested := make([]string, 0, 1)
			cellScope := scope + "/" + assetID + "/r" + strconv.Itoa(rowNumber) + "c" + strconv.Itoa(column)
			if err := r.tableCellContent(cell, part, cellScope, &cellText, &nested, assetID, rowNumber, column); err != nil {
				return "", err
			}
			if continuation {
				anchorIndex, ok := active[column]
				if !ok || anchorIndex >= len(r.assets[assetIndex].Table.Cells) ||
					r.assets[assetIndex].Table.Cells[anchorIndex].ColSpan != span {
					return "", fmt.Errorf("Broken vertical table merge: %s, row %d", assetID, rowNumber+1)
				}
				if len(cellText) > 0 || len(nested) > 0 {
					return "", fmt.Errorf("Unexpected content in merged continuation: %s", assetID)
				}
				r.assets[assetIndex].Table.Cells[anchorIndex].RowSpan++
				nextActive[column] = anchorIndex
				values = append(values, make([]string, span)...)
			} else {
				value := strings.Join(cellText, "\n")
				r.assets[assetIndex].Table.Cells = append(r.assets[assetIndex].Table.Cells, WordTableCellData{
					Row: rowNumber, Col: column, RowSpan: 1, ColSpan: span, Text: value, NestedTables: nested,
				})
				index := len(r.assets[assetIndex].Table.Cells) - 1
				values = append(values, value)
				values = append(values, make([]string, span-1)...)
				if vmerge != nil {
					nextActive[column] = index
				}
			}
			column += span
		}
		active = nextActive
		if after := row.findPath(wordName("trPr"), wordName("gridAfter")); after != nil {
			value, err := wordInt(after, "val", 0)
			if err != nil {
				return "", err
			}
			values = append(values, make([]string, value)...)
		}
		if len(values) > columns {
			columns = len(values)
		}
		r.assets[assetIndex].Table.Rows = append(r.assets[assetIndex].Table.Rows, values)
		if row.findPath(wordName("trPr"), wordName("tblHeader")) != nil {
			r.assets[assetIndex].Table.HeaderRows = append(r.assets[assetIndex].Table.HeaderRows, cloneStrings(values))
		}
	}
	table = r.assets[assetIndex].Table
	if len(table.Rows) == 0 || columns == 0 {
		return "", fmt.Errorf("Empty Word table: %s", assetID)
	}
	for index := range table.Rows {
		if missing := columns - len(table.Rows[index]); missing > 0 {
			table.Rows[index] = append(table.Rows[index], make([]string, missing)...)
		}
	}
	table.NumRows, table.NumCols = len(table.Rows), columns
	table.Headers = cloneStrings(table.Rows[0])
	if len(table.HeaderRows) == 0 {
		table.HeaderRows = [][]string{cloneStrings(table.Headers)}
	}
	return assetID, nil
}

// tableCellContent ports the cell content closure in WordReader.read_table.
func (r *wordReader) tableCellContent(container *xmlNode, part, cellScope string,
	cellText, nested *[]string, assetID string, rowNumber, column int) error {
	for _, child := range container.Children {
		switch {
		case isElement(child, nsWord, "p"):
			value, err := r.readParagraph(child, part, cellScope, false)
			if err != nil {
				return err
			}
			if value != "" {
				*cellText = append(*cellText, value)
			}
		case isElement(child, nsWord, "tbl"):
			nestedID, err := r.readTable(child, part, cellScope, &cellRef{assetID: assetID, row: rowNumber, col: column})
			if err != nil {
				return err
			}
			*nested = append(*nested, nestedID)
		case isElement(child, nsWord, "sdt"), isElement(child, nsWord, "sdtContent"),
			isElement(child, nsWord, "ins"), isElement(child, nsWord, "moveTo"),
			isElement(child, nsWord, "customXml"):
			if err := r.tableCellContent(child, part, cellScope, cellText, nested, assetID, rowNumber, column); err != nil {
				return err
			}
		case isElement(child, nsWord, "altChunk"), isElement(child, nsWord, "object"):
			return fmt.Errorf("Unsupported embedded content in %s", assetID)
		}
	}
	return nil
}

// splitNULSegments mirrors re.split(r"(\x00[^\x00]+\x00)", raw) with the
// captured markers retained.
func splitNULSegments(raw string) []string {
	locations := nulPlaceholder.FindAllStringIndex(raw, -1)
	if len(locations) == 0 {
		return []string{raw}
	}
	segments := make([]string, 0, len(locations)*2+1)
	last := 0
	for _, location := range locations {
		if location[0] > last {
			segments = append(segments, raw[last:location[0]])
		}
		segments = append(segments, raw[location[0]:location[1]])
		last = location[1]
	}
	if last < len(raw) {
		segments = append(segments, raw[last:])
	}
	return segments
}

// descendantsNamed mirrors the `children(element, wanted)` helper in
// WordReader.read_table: direct children of the wanted tag, descending only
// through wrapper elements.
func descendantsNamed(element *xmlNode, namespace, local string, wrappers []xmlName) []*xmlNode {
	result := make([]*xmlNode, 0, 8)
	var visit func(*xmlNode)
	visit = func(parent *xmlNode) {
		for _, child := range parent.Children {
			if isElement(child, namespace, local) {
				result = append(result, child)
				continue
			}
			for _, wrapper := range wrappers {
				if isElement(child, wrapper.namespace, wrapper.local) {
					visit(child)
					break
				}
			}
		}
	}
	visit(element)
	return result
}

// wordInt reads an integer Word attribute, defaulting when absent and
// rejecting invalid values the way Python's int() would.
func wordInt(node *xmlNode, local string, fallback int) (int, error) {
	value, ok := node.wordAttr(local)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid Word numeric attribute %s=%q", local, value)
	}
	return parsed, nil
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

// baseName is filepath.Base without importing path/filepath into the reader.
func baseName(path string) string {
	index := strings.LastIndexAny(path, `/\`)
	if index < 0 {
		return path
	}
	return path[index+1:]
}
