package document

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// headingStyle mirrors re.fullmatch(r"heading\s*([1-9])", name, re.I).
var headingStyle = regexp.MustCompile(`(?i)^heading\s*([1-9])$`)

// captionPrefix mirrors the prefix part of word_reader.CAPTION. The Python
// pattern ends with r"(?:\b|(?=[\s:：、.．]))"; the lookahead is handled by
// pythonWordBoundaryAfter because Go's regexp has no lookaround and its \b is
// ASCII-only.
var captionPrefix = regexp.MustCompile(`(?i)^\s*(?:图|表|figure|fig\.?|table)\s*(?:\d+(?:[.\-]\d+)*|[一二三四五六七八九十]+)`)

// noteLine mirrors word_reader.NOTE.
var noteLine = regexp.MustCompile(`(?i)^\s*(?:注|说明|来源|资料来源|note|notes|source)\s*[:：.]`)

var nulPlaceholder = regexp.MustCompile(`\x00[^\x00]+\x00`)

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

// tableCell is one logical table cell (merged continuations are folded into
// their anchor cell, matching word_reader.read_table).
type tableCell struct {
	row          int
	col          int
	rowSpan      int
	colSpan      int
	text         string
	nestedTables []string
}

// wordAsset is one picture or table asset discovered while reading the package.
type wordAsset struct {
	id         string
	kind       string // "table" or "picture"
	rows       [][]string
	cells      []tableCell
	headerRows [][]string
	headers    []string
	numRows    int
	numCols    int
	parentCell *cellRef
}

// wordBlock is one block before the document_readers manifest projection.
type wordBlock struct {
	ref      string
	kind     string
	text     string
	headings []string
	assetID  string
}

// wordReader ports rag_mcp.word_reader.WordReader: ordered OOXML extraction
// with explicit picture/table placeholders and fail-closed handling for
// structures that cannot be converted reliably.
//
// Deferred parity TODOs (documented in docs/GO_MIGRATION.md phase G3):
//   - Word formulas (m:oMath) are rejected instead of being converted to LaTeX
//     through docxlatex; the reader refuses the document with a clear error
//     rather than indexing a wrong formula.
//   - word_reader's asset caption/note association (WordReader.associate) is
//     not ported yet. Text extraction is unaffected for documents whose
//     captions/notes are ordinary blocks; the skipped-object pruning that runs
//     when a caption cannot be uniquely associated is not applied.
type wordReader struct {
	archive      *docxArchive
	contentTypes contentTypes
	blocks       []wordBlock
	assets       []*wordAsset
	assetIndex   map[string]int
	headings     []string
	paragraph    int
	section      int
	characters   int
	serial       int
	styles       map[string]styleInfo
	relations    map[string]map[string]relation
	visitedNotes map[string]struct{}
}

// parseDOCX extracts ordered blocks from one Word package. The caller has
// already validated the archive bounds (validateDOCX).
func parseDOCX(filePath string) ([]Block, error) {
	archive, err := openDOCX(filePath)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	reader := &wordReader{
		archive:      archive,
		assetIndex:   make(map[string]int),
		styles:       make(map[string]styleInfo),
		relations:    make(map[string]map[string]relation),
		visitedNotes: make(map[string]struct{}),
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
	return reader.documentBlocks(), nil
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

// readParagraph ports WordReader.read_paragraph, including the NUL-marker
// protocol used to splice textbox/note/picture blocks back into paragraph
// order.
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
	if emit {
		events := make(map[string]wordBlock, len(r.blocks)-offset)
		for _, block := range r.blocks[offset:] {
			events[block.ref] = block
		}
		r.blocks = r.blocks[:offset]
		kind := KindParagraph
		switch {
		case headingLevel > 0:
			kind = KindHeading
		case captionMatch(text):
			kind = KindCaption
		case styleMentionsCaption(styles):
			kind = KindCaption
		case noteLine.MatchString(text):
			kind = KindNote
		}
		for _, segment := range splitNULSegments(raw) {
			if strings.HasPrefix(segment, "\x00") {
				if event, ok := events[strings.Trim(segment, "\x00")]; ok {
					r.blocks = append(r.blocks, event)
				}
				continue
			}
			if trimmed := strings.TrimSpace(segment); trimmed != "" {
				if err := r.block(kind, trimmed, headings, ""); err != nil {
					return "", err
				}
			}
		}
	} else if text != "" {
		switch {
		case captionMatch(text):
			if err := r.block(KindCaption, text, headings, ""); err != nil {
				return "", err
			}
		case noteLine.MatchString(text):
			if err := r.block(KindNote, text, headings, ""); err != nil {
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
		return "", fmt.Errorf("Unsupported Word formula at %s, paragraph %d: the Go reader does not implement docxlatex formula conversion", part, r.paragraph)
	}
	if isElement(node, nsWord, "object") {
		return "", fmt.Errorf("Unsupported embedded OLE object at %s, paragraph %d", part, r.paragraph)
	}
	if isElement(node, nsDrawingML, "blip") || isElement(node, nsVML, "imagedata") {
		reference, err := r.picture(node, part)
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
			builder.WriteString("\x00" + block.ref + "\x00")
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
		builder.WriteString("\x00" + block.ref + "\x00")
	}
	return builder.String(), nil
}

// picture ports WordReader.picture: validate the relationship/part/content type
// and register a picture asset, returning the placeholder block ref.
func (r *wordReader) picture(node *xmlNode, part string) (string, error) {
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
	r.assetIndex[assetID] = len(r.assets)
	r.assets = append(r.assets, &wordAsset{id: assetID, kind: "picture"})
	if err := r.block(KindPicture, "", cloneStrings(r.headings), assetID); err != nil {
		return "", err
	}
	return r.blocks[len(r.blocks)-1].ref, nil
}

// readTable ports WordReader.read_table: gridBefore/gridSpan/gridAfter,
// vertical merges with anchor cells, nested tables, header rows and the
// rejection of legacy horizontal merges and invalid grids.
func (r *wordReader) readTable(node *xmlNode, part, scope string, parentCell *cellRef) (string, error) {
	assetID := "table-" + strconv.Itoa(len(r.assets)+1)
	headings := cloneStrings(r.headings)
	asset := &wordAsset{id: assetID, kind: "table", parentCell: parentCell}
	assetIndex := len(r.assets)
	r.assetIndex[assetID] = assetIndex
	r.assets = append(r.assets, asset)
	if err := r.block("table", "", headings, assetID); err != nil {
		return "", err
	}
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
				if !ok || anchorIndex >= len(r.assets[assetIndex].cells) ||
					r.assets[assetIndex].cells[anchorIndex].colSpan != span {
					return "", fmt.Errorf("Broken vertical table merge: %s, row %d", assetID, rowNumber+1)
				}
				if len(cellText) > 0 || len(nested) > 0 {
					return "", fmt.Errorf("Unexpected content in merged continuation: %s", assetID)
				}
				r.assets[assetIndex].cells[anchorIndex].rowSpan++
				nextActive[column] = anchorIndex
				values = append(values, make([]string, span)...)
			} else {
				value := strings.Join(cellText, "\n")
				r.assets[assetIndex].cells = append(r.assets[assetIndex].cells, tableCell{
					row: rowNumber, col: column, rowSpan: 1, colSpan: span, text: value, nestedTables: nested,
				})
				index := len(r.assets[assetIndex].cells) - 1
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
		r.assets[assetIndex].rows = append(r.assets[assetIndex].rows, values)
		if row.findPath(wordName("trPr"), wordName("tblHeader")) != nil {
			r.assets[assetIndex].headerRows = append(r.assets[assetIndex].headerRows, cloneStrings(values))
		}
	}
	table := r.assets[assetIndex]
	if len(table.rows) == 0 || columns == 0 {
		return "", fmt.Errorf("Empty Word table: %s", assetID)
	}
	for index := range table.rows {
		if missing := columns - len(table.rows[index]); missing > 0 {
			table.rows[index] = append(table.rows[index], make([]string, missing)...)
		}
	}
	table.numRows, table.numCols = len(table.rows), columns
	table.headers = cloneStrings(table.rows[0])
	if len(table.headerRows) == 0 {
		table.headerRows = [][]string{cloneStrings(table.headers)}
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

// block ports WordReader.block: bounded append with a stable serial ref.
func (r *wordReader) block(kind, text string, headings []string, assetID string) error {
	r.characters += utf8.RuneCountInString(text)
	if r.characters > MaxExtractedCharacters {
		return fmt.Errorf("Word extracted text exceeds the size limit")
	}
	r.serial++
	r.blocks = append(r.blocks, wordBlock{
		ref:      "block-" + strconv.Itoa(r.serial),
		kind:     kind,
		text:     text,
		headings: cloneStrings(headings),
		assetID:  assetID,
	})
	return nil
}

// documentBlocks projects the manifest blocks the way
// document_readers._docx_blocks does: table assets expand into one row block
// each, picture assets become "[Image: <asset_id>]" and empty text blocks are
// dropped.
func (r *wordReader) documentBlocks() []Block {
	result := make([]Block, 0, len(r.blocks))
	for _, block := range r.blocks {
		if block.assetID != "" {
			index, ok := r.assetIndex[block.assetID]
			if !ok {
				continue
			}
			asset := r.assets[index]
			if asset.kind == "table" {
				for _, row := range asset.rows {
					result = append(result, Block{
						Text:     tableRowText(row),
						Kind:     KindTableRow,
						Headings: cloneStrings(block.headings),
						Table:    index + 1,
					})
				}
				continue
			}
			result = append(result, Block{
				Text:     "[Image: " + asset.id + "]",
				Kind:     KindPicture,
				Headings: cloneStrings(block.headings),
			})
			continue
		}
		if block.text != "" {
			result = append(result, Block{
				Text:     block.text,
				Kind:     block.kind,
				Headings: cloneStrings(block.headings),
			})
		}
	}
	return result
}

// tableRowText mirrors document_readers._docx_blocks' row rendering.
func tableRowText(row []string) string {
	cells := make([]string, len(row))
	for index, cell := range row {
		cell = strings.ReplaceAll(cell, "\n", " ")
		cell = strings.ReplaceAll(cell, "|", `\|`)
		cells[index] = cell
	}
	return strings.Join(cells, " | ")
}

// joinDOCX mirrors document_readers._join_docx.
func joinDOCX(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	previousTable := 0
	for _, block := range blocks {
		if len(parts) > 0 {
			if block.Table != 0 && block.Table == previousTable {
				parts = append(parts, "\n")
			} else {
				parts = append(parts, "\n\n")
			}
		}
		parts = append(parts, block.Text)
		previousTable = block.Table
	}
	return strings.Join(parts, "")
}

// captionMatch mirrors word_reader.CAPTION.match using a Python-compatible
// Unicode word boundary (Go's regexp \b is ASCII-only and has no lookahead).
func captionMatch(text string) bool {
	match := captionPrefix.FindStringIndex(text)
	if match == nil {
		return false
	}
	rest := text[match[1]:]
	if rest == "" {
		return true
	}
	runeValue, _ := utf8.DecodeRuneInString(rest)
	return !isPythonWordRune(runeValue)
}

// isPythonWordRune reports whether rune counts as \w for Python's re module.
func isPythonWordRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value) ||
		unicode.Is(unicode.Nl, value) || unicode.Is(unicode.No, value)
}

func styleMentionsCaption(styles []string) bool {
	for _, style := range styles {
		if strings.Contains(strings.ToLower(style), "caption") {
			return true
		}
	}
	return false
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
