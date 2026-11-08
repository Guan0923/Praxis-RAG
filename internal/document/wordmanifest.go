package document

import (
	"strings"
)

// WordParserVersion mirrors word_reader.WORD_PARSER_VERSION. Parsed Word content
// is only reusable while the manifest reports this version.
const WordParserVersion = "word-assets-v1"

// WordLocation is the ordered position of a Word block/asset inside the package.
// It mirrors word_reader.WordReader.location; the fields feed caption/note
// association and the artifact manifest.
type WordLocation struct {
	Part      string   `json:"part"`
	Paragraph int      `json:"paragraph"`
	Headings  []string `json:"headings"`
	Scope     string   `json:"scope"`
	Section   int      `json:"section"`
}

// WordManifestBlock is one ordered block of a Word manifest.
type WordManifestBlock struct {
	Ref              string       `json:"ref"`
	Order            int          `json:"order"`
	Kind             string       `json:"kind"`
	Text             string       `json:"text"`
	Location         WordLocation `json:"location"`
	AssetID          string       `json:"asset_id,omitempty"`
	InCell           bool         `json:"in_cell,omitempty"`
	Bookmarks        []string     `json:"bookmarks,omitempty"`
	References       []string     `json:"references,omitempty"`
	AssociatedAssets []string     `json:"associated_assets,omitempty"`
}

// WordAnnotation is one attached caption or footnote.
type WordAnnotation struct {
	Text     string       `json:"text"`
	Ref      string       `json:"ref"`
	Location WordLocation `json:"location"`
}

// WordTableCellData is one logical table cell; merged continuations are folded
// into the anchor cell, like word_reader.read_table.
type WordTableCellData struct {
	Row          int      `json:"row"`
	Col          int      `json:"col"`
	RowSpan      int      `json:"row_span"`
	ColSpan      int      `json:"col_span"`
	Text         string   `json:"text"`
	NestedTables []string `json:"nested_tables"`
}

// WordParentCell identifies the outer cell that contains a nested table.
type WordParentCell struct {
	AssetID string `json:"asset_id"`
	Row     int    `json:"row"`
	Col     int    `json:"col"`
}

// WordTableData is the table structure exported by the Word reader. Caption and
// NestedTables are filled by the asset pipeline (word_assets.parse_word), not by
// the OOXML parse itself.
type WordTableData struct {
	Rows         [][]string                `json:"rows"`
	Cells        []WordTableCellData       `json:"cells"`
	Headers      []string                  `json:"headers"`
	HeaderRows   [][]string                `json:"header_rows"`
	NumRows      int                       `json:"num_rows"`
	NumCols      int                       `json:"num_cols"`
	Footnotes    []string                  `json:"footnotes"`
	ParentCell   *WordParentCell           `json:"parent_cell"`
	Caption      string                    `json:"caption,omitempty"`
	NestedTables map[string]*WordTableData `json:"nested_tables,omitempty"`
}

// WordManifestAsset is one picture or table asset of the manifest. Image bytes
// are kept out of the manifest (see WordPackage.Images) so the manifest remains
// a plain JSON artifact, matching the Python parser.
type WordManifestAsset struct {
	AssetID              string           `json:"asset_id"`
	Kind                 string           `json:"kind"`
	ImageFile            *string          `json:"image_file"`
	MIMEType             *string          `json:"mime_type"`
	Caption              string           `json:"caption"`
	CaptionSource        string           `json:"caption_source"`
	GeneratedDescription string           `json:"generated_description"`
	Captions             []WordAnnotation `json:"captions"`
	Footnotes            []WordAnnotation `json:"footnotes"`
	Table                *WordTableData   `json:"table"`
	Location             WordLocation     `json:"location"`
	Labels               []string         `json:"labels"`
	Bookmarks            []string         `json:"bookmarks"`
}

// WordDiagnostic records a safe, structured parse/association diagnostic.
type WordDiagnostic struct {
	Kind     string       `json:"kind"`
	Message  string       `json:"message"`
	Location WordLocation `json:"location"`
	Text     string       `json:"text"`
	AssetIDs []string     `json:"asset_ids"`
}

// WordManifest is the ordered OOXML extraction result. Its JSON shape matches
// word_reader.parse_docx so runtime artifacts and the external vision worker
// stay compatible.
type WordManifest struct {
	Version            string               `json:"version"`
	Blocks             []WordManifestBlock  `json:"blocks"`
	Assets             []*WordManifestAsset `json:"assets"`
	Diagnostics        []WordDiagnostic     `json:"diagnostics"`
	SkippedObjectCount int                  `json:"skipped_object_count"`
}

// WordPackage is a parsed Word document: the manifest plus image bytes keyed by
// manifest asset id. It is the storage-decoupled output of ParseWordPackage.
type WordPackage struct {
	Manifest WordManifest
	Images   map[string][]byte
}

// DocumentBlocks projects the manifest the way document_readers._docx_blocks
// does: table assets expand to one row block each, picture assets become
// "[Image: <asset_id>]" and empty text blocks are dropped.
func (m WordManifest) DocumentBlocks() []Block {
	assets := make(map[string]*WordManifestAsset, len(m.Assets))
	assetIndex := make(map[string]int, len(m.Assets))
	for index, asset := range m.Assets {
		assets[asset.AssetID] = asset
		assetIndex[asset.AssetID] = index
	}
	result := make([]Block, 0, len(m.Blocks))
	for _, block := range m.Blocks {
		if block.AssetID != "" {
			asset, ok := assets[block.AssetID]
			if !ok {
				continue
			}
			if asset.Kind == "table" && asset.Table != nil {
				tableNumber := assetIndex[block.AssetID] + 1
				for _, row := range asset.Table.Rows {
					result = append(result, Block{
						Text:     tableRowText(row),
						Kind:     KindTableRow,
						Headings: cloneStrings(block.Location.Headings),
						Table:    tableNumber,
					})
				}
				continue
			}
			result = append(result, Block{
				Text:     "[Image: " + asset.AssetID + "]",
				Kind:     KindPicture,
				Headings: cloneStrings(block.Location.Headings),
			})
			continue
		}
		if block.Text != "" {
			result = append(result, Block{
				Text:     block.Text,
				Kind:     block.Kind,
				Headings: cloneStrings(block.Location.Headings),
			})
		}
	}
	return result
}

// Render ports word_reader.render_word: ordered plain text for the document.md
// artifact, using table rows and picture placeholders and skipping in-cell
// caption/note blocks.
func (m WordManifest) Render() string {
	assets := make(map[string]*WordManifestAsset, len(m.Assets))
	for _, asset := range m.Assets {
		assets[asset.AssetID] = asset
	}
	parts := make([]string, 0, len(m.Blocks))
	for _, block := range m.Blocks {
		asset := assets[block.AssetID]
		if asset != nil {
			if asset.Kind == "table" && asset.Table != nil {
				rows := make([]string, len(asset.Table.Rows))
				for index, row := range asset.Table.Rows {
					rows[index] = tableRowText(row)
				}
				parts = append(parts, strings.Join(rows, "\n"))
			} else {
				parts = append(parts, "[Image: "+asset.AssetID+"]")
			}
			continue
		}
		if block.Text != "" && !block.InCell {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n")
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

// joinDOCX mirrors document_readers._join_docx over projected blocks.
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
