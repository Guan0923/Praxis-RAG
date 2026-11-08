package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Asset is one storage-decoupled document asset produced by the Word/PDF asset
// pipelines. Runtime persists Data (pictures) at Name (a document-relative blob
// key) and stores Payload(source) as the asset record.
//
// Identity/ordering/dedup rules (matching the Python pipelines):
//   - ID is document-scoped: "<document_id>:<parser asset id>", so the same
//     bytes appearing twice remain two distinct assets.
//   - Order is the manifest asset order; asset ids are assigned in creation
//     order by the parser.
//   - No content deduplication is performed at this layer. Table image files
//     are not exposed as assets for PDF (the worker reports image_file null for
//     tables), matching pdf_assets.
//   - SHA256/Size describe Data for pictures; they are empty/zero for tables.
type Asset struct {
	DocumentID string
	ID         string
	SourceID   string
	Kind       string
	Storage    string // "word" or "pdf"
	// Name is the document-relative artifact key (for example
	// "<document_id>/images/picture-1.png"). Empty for table assets.
	Name string
	// SourceName is the parser-relative image path inside the staging directory
	// (for example "images/picture-1.png"). Empty for table assets.
	SourceName string
	MIMEType   string
	Data       []byte
	SHA256     string
	Size       int64
	Order      int
	Page       int
	BBox       map[string]any
	Location   WordLocation

	Caption              string
	CaptionSource        string
	GeneratedDescription string
	Captions             []AssetAnnotation
	Footnotes            []AssetAnnotation
	Table                *AssetTable
	Labels               []string
	Bookmarks            []string
}

// AssetAnnotation is one caption or footnote attached to an asset.
type AssetAnnotation struct {
	Ref       string
	Text      string
	Page      int
	BBox      map[string]any
	Locations []PDFLocation
	// Location is set for Word annotations only.
	Location *WordLocation
}

// AssetParentCell identifies the outer cell of a nested Word table.
type AssetParentCell struct {
	AssetID string
	Row     int
	Col     int
}

// AssetTable is the storage-neutral table payload for both storages. Rows and
// headers are typed; Cells keep the parser payload shape (Word and PDF cells
// expose different provenance fields).
type AssetTable struct {
	NumRows      int
	NumCols      int
	Orientation  string
	Headers      []string
	HeaderRows   [][]string
	Rows         [][]string
	Cells        []map[string]any
	Footnotes    []string
	ParentCell   *AssetParentCell
	NestedTables map[string]*AssetTable
	Caption      string
}

// WordContent is the storage-decoupled Word pipeline output: the qualified
// manifest (document-scoped ids), the document.md text, and asset records.
type WordContent struct {
	DocumentID         string
	Manifest           WordManifest
	DocumentMarkdown   string
	Assets             []Asset
	Diagnostics        []WordDiagnostic
	SkippedObjectCount int
}

// BuildWordContent applies the word_assets.parse_word identity step: asset ids
// and image paths become document-relative, table captions/nested tables are
// linked, and typed asset records (with bytes and checksums) are produced.
func BuildWordContent(documentID string, pkg *WordPackage) (*WordContent, error) {
	if pkg == nil {
		return nil, fmt.Errorf("Word package is missing")
	}
	ids := make(map[string]string, len(pkg.Manifest.Assets))
	for _, asset := range pkg.Manifest.Assets {
		ids[asset.AssetID] = documentID + ":" + asset.AssetID
	}
	qualified := cloneWordManifest(pkg.Manifest)
	for _, asset := range qualified.Assets {
		sourceID := asset.AssetID
		if qualifiedID, ok := ids[sourceID]; ok {
			asset.AssetID = qualifiedID
		}
		if asset.ImageFile != nil {
			path := documentID + "/" + *asset.ImageFile
			asset.ImageFile = &path
		}
		if asset.Table != nil {
			if asset.Table.ParentCell != nil {
				if qualifiedID, ok := ids[asset.Table.ParentCell.AssetID]; ok {
					asset.Table.ParentCell.AssetID = qualifiedID
				}
			}
			for index := range asset.Table.Cells {
				rewritten := make([]string, 0, len(asset.Table.Cells[index].NestedTables))
				for _, nestedID := range asset.Table.Cells[index].NestedTables {
					if qualifiedID, ok := ids[nestedID]; ok {
						rewritten = append(rewritten, qualifiedID)
					}
				}
				asset.Table.Cells[index].NestedTables = rewritten
			}
		}
	}
	for index := range qualified.Blocks {
		block := &qualified.Blocks[index]
		if block.AssetID != "" {
			if qualifiedID, ok := ids[block.AssetID]; ok {
				block.AssetID = qualifiedID
			}
		}
		if len(block.AssociatedAssets) > 0 {
			rewritten := make([]string, 0, len(block.AssociatedAssets))
			for _, assetID := range block.AssociatedAssets {
				if qualifiedID, ok := ids[assetID]; ok {
					rewritten = append(rewritten, qualifiedID)
				}
			}
			block.AssociatedAssets = rewritten
		}
	}
	// Link table captions and directly nested tables, like word_assets.
	for _, asset := range qualified.Assets {
		if asset.Table == nil {
			continue
		}
		asset.Table.Caption = asset.Caption
		nested := make(map[string]*WordTableData)
		for _, candidate := range qualified.Assets {
			if candidate.Table == nil || candidate.Table.ParentCell == nil {
				continue
			}
			if candidate.Table.ParentCell.AssetID == asset.AssetID {
				nested[candidate.AssetID] = candidate.Table
			}
		}
		asset.Table.NestedTables = nested
	}
	assets := make([]Asset, 0, len(qualified.Assets))
	for order, asset := range qualified.Assets {
		record := Asset{
			DocumentID:           documentID,
			ID:                   asset.AssetID,
			SourceID:             sourceAssetID(asset.AssetID, documentID),
			Kind:                 asset.Kind,
			Storage:              "word",
			Order:                order,
			Location:             cloneWordLocation(asset.Location),
			Caption:              asset.Caption,
			CaptionSource:        asset.CaptionSource,
			GeneratedDescription: asset.GeneratedDescription,
			Captions:             wordAnnotations(asset.Captions),
			Footnotes:            wordAnnotations(asset.Footnotes),
			Labels:               cloneStrings(asset.Labels),
			Bookmarks:            cloneStrings(asset.Bookmarks),
		}
		if asset.MIMEType != nil {
			record.MIMEType = *asset.MIMEType
		}
		if asset.ImageFile != nil {
			record.Name = *asset.ImageFile
			record.SourceName = strings.TrimPrefix(*asset.ImageFile, documentID+"/")
			data := pkg.Images[record.SourceID]
			if asset.Kind == "picture" {
				if len(data) == 0 {
					return nil, fmt.Errorf("Word picture data is missing: %s", record.SourceID)
				}
				record.Data = data
				record.SHA256, record.Size = digestBytes(data)
			}
		}
		if asset.Table != nil {
			record.Table = wordAssetTable(asset.Table)
		}
		assets = append(assets, record)
	}
	return &WordContent{
		DocumentID:         documentID,
		Manifest:           qualified,
		DocumentMarkdown:   qualified.Render(),
		Assets:             assets,
		Diagnostics:        qualified.Diagnostics,
		SkippedObjectCount: qualified.SkippedObjectCount,
	}, nil
}

// PDFContent is the storage-decoupled PDF pipeline output: the qualified
// manifest (document-scoped asset ids), the document.md text, and asset records.
type PDFContent struct {
	DocumentID       string
	Manifest         PDFManifest
	DocumentMarkdown string
	Assets           []Asset
}

// BuildPDFContent applies the pdf_assets.parse_pdf identity step: asset ids and
// image paths become document-relative, image bytes are read from the parser
// staging directory, and typed asset records are produced.
func BuildPDFContent(manifest PDFManifest, documentID, stagingDir string) (*PDFContent, error) {
	qualified := clonePDFManifest(manifest)
	ids := make(map[string]string, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		ids[asset.AssetID] = documentID + ":" + asset.AssetID
	}
	for index := range qualified.Assets {
		asset := &qualified.Assets[index]
		if qualifiedID, ok := ids[asset.AssetID]; ok {
			asset.AssetID = qualifiedID
		}
		if asset.ImageFile != "" {
			asset.ImageFile = documentID + "/" + asset.ImageFile
		}
	}
	for index := range qualified.Blocks {
		block := &qualified.Blocks[index]
		if block.AssetID != "" {
			if qualifiedID, ok := ids[block.AssetID]; ok {
				block.AssetID = qualifiedID
			}
		}
	}
	assets := make([]Asset, 0, len(manifest.Assets))
	for order, asset := range manifest.Assets {
		record := Asset{
			DocumentID:           documentID,
			ID:                   ids[asset.AssetID],
			SourceID:             asset.AssetID,
			Kind:                 asset.Kind,
			Storage:              "pdf",
			Order:                order,
			Page:                 asset.Page,
			BBox:                 asset.BBox,
			Caption:              asset.Caption,
			CaptionSource:        asset.CaptionSource,
			GeneratedDescription: asset.GeneratedDescription,
			Captions:             pdfAnnotations(asset.Captions),
			Footnotes:            pdfAnnotations(asset.Footnotes),
		}
		if asset.ImageFile != "" {
			record.SourceName = asset.ImageFile
			record.Name = documentID + "/" + asset.ImageFile
			data, err := os.ReadFile(filepath.Join(stagingDir, filepath.FromSlash(asset.ImageFile)))
			if err != nil {
				return nil, fmt.Errorf("PDF asset image is missing: %s", asset.ImageFile)
			}
			if len(data) == 0 {
				return nil, fmt.Errorf("PDF asset image is empty: %s", asset.ImageFile)
			}
			record.MIMEType = "image/png"
			record.Data = data
			record.SHA256, record.Size = digestBytes(data)
		}
		if asset.Table != nil {
			record.Table = pdfAssetTable(asset.Table)
		}
		assets = append(assets, record)
	}
	qualifiedMarkdown, err := RenderContent(qualified)
	if err != nil {
		return nil, err
	}
	return &PDFContent{
		DocumentID:       documentID,
		Manifest:         qualified,
		DocumentMarkdown: qualifiedMarkdown,
		Assets:           assets,
	}, nil
}

// WriteArtifacts writes the qualified manifest.json and document.md artifacts
// for the Word pipeline, mirroring word_assets.parse_word's render step.
func (c *WordContent) WriteArtifacts(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("Word artifact directory is not writable: %v", err)
	}
	if err := c.Manifest.WriteFile(filepath.Join(dir, "manifest.json")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "document.md"), []byte(c.DocumentMarkdown), 0o644); err != nil {
		return fmt.Errorf("Word document.md is not writable: %v", err)
	}
	return nil
}

// WriteDocumentMarkdown writes the document.md artifact for the PDF pipeline
// (the worker already wrote manifest.json into the staging directory).
func (c *PDFContent) WriteDocumentMarkdown(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("PDF artifact directory is not writable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "document.md"), []byte(c.DocumentMarkdown), 0o644); err != nil {
		return fmt.Errorf("PDF document.md is not writable: %v", err)
	}
	return nil
}

// ValidateWordParserVersion mirrors word_assets.read_word_content's parser
// version guard.
func ValidateWordParserVersion(version string) error {
	if version != WordParserVersion {
		return fmt.Errorf("Word parser changed; reimport this document")
	}
	return nil
}

// ValidatePDFParserVersion mirrors pdf_assets.read_pdf_content's parser version
// guard.
func ValidatePDFParserVersion(version string) error {
	if version != PDFParserVersion {
		return fmt.Errorf("PDF parser version changed; explicitly reimport this PDF")
	}
	return nil
}

// Payload renders the asset record the state/Qdrant layers store. The shape
// matches word_assets.WordAsset.payload / pdf_assets.PdfAsset.payload.
func (a Asset) Payload(source string) map[string]any {
	if a.Storage == "word" {
		payload := map[string]any{
			"asset_id":              a.ID,
			"kind":                  a.Kind,
			"image_file":            optionalString(a.Name),
			"mime_type":             optionalString(a.MIMEType),
			"caption":               a.Caption,
			"caption_source":        a.CaptionSource,
			"generated_description": a.GeneratedDescription,
			"captions":              wordAnnotationPayloads(a.Captions),
			"footnotes":             wordAnnotationPayloads(a.Footnotes),
			"table":                 wordTablePayload(a.Table),
			"location":              wordLocationPayload(a.Location),
			"labels":                cloneStrings(a.Labels),
			"bookmarks":             cloneStrings(a.Bookmarks),
			"record_type":           "asset",
			"document_id":           a.DocumentID,
			"source":                source,
			"asset_kind":            a.Kind,
			"asset_storage":         "word",
		}
		if payload["labels"] == nil {
			payload["labels"] = []string{}
		}
		if payload["bookmarks"] == nil {
			payload["bookmarks"] = []string{}
		}
		return payload
	}
	payload := map[string]any{
		"record_type":           "asset",
		"document_id":           a.DocumentID,
		"asset_id":              a.ID,
		"asset_kind":            a.Kind,
		"source":                source,
		"page":                  a.Page,
		"bbox":                  a.BBox,
		"caption":               a.Caption,
		"caption_source":        a.CaptionSource,
		"generated_description": a.GeneratedDescription,
		"image_file":            optionalString(a.Name),
		"table":                 pdfTablePayload(a.Table),
		"captions":              pdfAnnotationPayloads(a.Captions),
		"footnotes":             pdfAnnotationPayloads(a.Footnotes),
	}
	return payload
}

func digestBytes(data []byte) (string, int64) {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), int64(len(data))
}

func optionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// sourceAssetID recovers the parser-relative asset id from a qualified id.
func sourceAssetID(qualifiedID, documentID string) string {
	return strings.TrimPrefix(qualifiedID, documentID+":")
}

func wordAnnotations(annotations []WordAnnotation) []AssetAnnotation {
	result := make([]AssetAnnotation, 0, len(annotations))
	for _, annotation := range annotations {
		location := annotation.Location
		result = append(result, AssetAnnotation{
			Ref: annotation.Ref, Text: annotation.Text, Location: &location,
		})
	}
	return result
}

func pdfAnnotations(annotations []PDFAnnotation) []AssetAnnotation {
	result := make([]AssetAnnotation, 0, len(annotations))
	for _, annotation := range annotations {
		result = append(result, AssetAnnotation{
			Ref: annotation.Ref, Text: annotation.Text, Page: annotation.Page,
			BBox: annotation.BBox, Locations: annotation.Locations,
		})
	}
	return result
}

func wordAssetTable(table *WordTableData) *AssetTable {
	asset := &AssetTable{
		NumRows:      table.NumRows,
		NumCols:      table.NumCols,
		Headers:      cloneStrings(table.Headers),
		HeaderRows:   cloneRows(table.HeaderRows),
		Rows:         cloneRows(table.Rows),
		Footnotes:    cloneStrings(table.Footnotes),
		Caption:      table.Caption,
		NestedTables: make(map[string]*AssetTable, len(table.NestedTables)),
	}
	for _, cell := range table.Cells {
		cells := map[string]any{
			"row":           cell.Row,
			"col":           cell.Col,
			"row_span":      cell.RowSpan,
			"col_span":      cell.ColSpan,
			"text":          cell.Text,
			"nested_tables": cloneStrings(cell.NestedTables),
		}
		asset.Cells = append(asset.Cells, cells)
	}
	if table.ParentCell != nil {
		asset.ParentCell = &AssetParentCell{
			AssetID: table.ParentCell.AssetID, Row: table.ParentCell.Row, Col: table.ParentCell.Col,
		}
	}
	for assetID, nested := range table.NestedTables {
		if nested == nil {
			continue
		}
		asset.NestedTables[assetID] = wordAssetTable(nested)
	}
	return asset
}

func pdfAssetTable(table *PDFTable) *AssetTable {
	asset := &AssetTable{
		NumRows:     table.NumRows,
		NumCols:     table.NumCols,
		Orientation: table.Orientation,
		Headers:     cloneStrings(table.Headers),
		HeaderRows:  cloneRows(table.HeaderRows),
		Rows:        cloneRows(table.Rows),
		Footnotes:   cloneStrings(table.Footnotes),
		Cells:       table.Cells,
	}
	return asset
}

func wordLocationPayload(location WordLocation) map[string]any {
	headings := cloneStrings(location.Headings)
	if headings == nil {
		headings = []string{}
	}
	return map[string]any{
		"part":      location.Part,
		"paragraph": location.Paragraph,
		"headings":  headings,
		"scope":     location.Scope,
		"section":   location.Section,
	}
}

func wordAnnotationPayloads(annotations []AssetAnnotation) []map[string]any {
	result := make([]map[string]any, 0, len(annotations))
	for _, annotation := range annotations {
		location := WordLocation{}
		if annotation.Location != nil {
			location = *annotation.Location
		}
		result = append(result, map[string]any{
			"text":     annotation.Text,
			"ref":      annotation.Ref,
			"location": wordLocationPayload(location),
		})
	}
	return result
}

func pdfAnnotationPayloads(annotations []AssetAnnotation) []map[string]any {
	result := make([]map[string]any, 0, len(annotations))
	for _, annotation := range annotations {
		locations := make([]map[string]any, 0, len(annotation.Locations))
		for _, location := range annotation.Locations {
			locations = append(locations, map[string]any{
				"page": location.Page, "bbox": location.BBox, "charspan": location.Charspan,
			})
		}
		result = append(result, map[string]any{
			"ref":       annotation.Ref,
			"text":      annotation.Text,
			"page":      annotation.Page,
			"bbox":      annotation.BBox,
			"locations": locations,
		})
	}
	return result
}

func wordTablePayload(table *AssetTable) any {
	if table == nil {
		return nil
	}
	nested := make(map[string]any, len(table.NestedTables))
	for assetID, child := range table.NestedTables {
		nested[assetID] = wordTablePayload(child)
	}
	cells := table.Cells
	if cells == nil {
		cells = []map[string]any{}
	}
	footnotes := cloneStrings(table.Footnotes)
	if footnotes == nil {
		footnotes = []string{}
	}
	headers := cloneStrings(table.Headers)
	if headers == nil {
		headers = []string{}
	}
	var parent any
	if table.ParentCell != nil {
		parent = map[string]any{
			"asset_id": table.ParentCell.AssetID,
			"row":      table.ParentCell.Row,
			"col":      table.ParentCell.Col,
		}
	}
	return map[string]any{
		"rows":          cloneRows(table.Rows),
		"cells":         cells,
		"headers":       headers,
		"header_rows":   cloneRows(table.HeaderRows),
		"num_rows":      table.NumRows,
		"num_cols":      table.NumCols,
		"footnotes":     footnotes,
		"parent_cell":   parent,
		"caption":       table.Caption,
		"nested_tables": nested,
	}
}

func pdfTablePayload(table *AssetTable) any {
	if table == nil {
		return nil
	}
	cells := table.Cells
	if cells == nil {
		cells = []map[string]any{}
	}
	footnotes := cloneStrings(table.Footnotes)
	if footnotes == nil {
		footnotes = []string{}
	}
	return map[string]any{
		"num_rows":    table.NumRows,
		"num_cols":    table.NumCols,
		"orientation": table.Orientation,
		"headers":     cloneStrings(table.Headers),
		"header_rows": cloneRows(table.HeaderRows),
		"rows":        cloneRows(table.Rows),
		"cells":       cells,
		"footnotes":   footnotes,
	}
}

func cloneRows(rows [][]string) [][]string {
	if rows == nil {
		return nil
	}
	cloned := make([][]string, len(rows))
	for index, row := range rows {
		cloned[index] = cloneStrings(row)
	}
	return cloned
}

func cloneWordLocation(location WordLocation) WordLocation {
	location.Headings = cloneStrings(location.Headings)
	return location
}

// cloneWordManifest deep-copies the manifest so qualification does not mutate
// the parser output retained by the caller.
func cloneWordManifest(manifest WordManifest) WordManifest {
	cloned := WordManifest{
		Version:            manifest.Version,
		SkippedObjectCount: manifest.SkippedObjectCount,
	}
	cloned.Blocks = make([]WordManifestBlock, len(manifest.Blocks))
	for index, block := range manifest.Blocks {
		block.Location = cloneWordLocation(block.Location)
		block.Bookmarks = cloneStrings(block.Bookmarks)
		block.References = cloneStrings(block.References)
		block.AssociatedAssets = cloneStrings(block.AssociatedAssets)
		cloned.Blocks[index] = block
	}
	cloned.Assets = make([]*WordManifestAsset, len(manifest.Assets))
	for index, asset := range manifest.Assets {
		copied := *asset
		copied.Location = cloneWordLocation(asset.Location)
		copied.Labels = cloneStrings(asset.Labels)
		copied.Bookmarks = cloneStrings(asset.Bookmarks)
		copied.Captions = append([]WordAnnotation(nil), asset.Captions...)
		copied.Footnotes = append([]WordAnnotation(nil), asset.Footnotes...)
		if asset.ImageFile != nil {
			value := *asset.ImageFile
			copied.ImageFile = &value
		}
		if asset.MIMEType != nil {
			value := *asset.MIMEType
			copied.MIMEType = &value
		}
		if asset.Table != nil {
			copied.Table = cloneWordTable(asset.Table)
		}
		cloned.Assets[index] = &copied
	}
	cloned.Diagnostics = make([]WordDiagnostic, len(manifest.Diagnostics))
	for index, diagnostic := range manifest.Diagnostics {
		diagnostic.Location = cloneWordLocation(diagnostic.Location)
		diagnostic.AssetIDs = cloneStrings(diagnostic.AssetIDs)
		cloned.Diagnostics[index] = diagnostic
	}
	return cloned
}

func cloneWordTable(table *WordTableData) *WordTableData {
	copied := &WordTableData{
		Rows:       cloneRows(table.Rows),
		Headers:    cloneStrings(table.Headers),
		HeaderRows: cloneRows(table.HeaderRows),
		NumRows:    table.NumRows,
		NumCols:    table.NumCols,
		Footnotes:  cloneStrings(table.Footnotes),
		Caption:    table.Caption,
		Cells:      make([]WordTableCellData, len(table.Cells)),
	}
	for index, cell := range table.Cells {
		cell.NestedTables = cloneStrings(cell.NestedTables)
		copied.Cells[index] = cell
	}
	if table.ParentCell != nil {
		parent := *table.ParentCell
		copied.ParentCell = &parent
	}
	copied.NestedTables = make(map[string]*WordTableData, len(table.NestedTables))
	for assetID, nested := range table.NestedTables {
		if nested == nil {
			continue
		}
		copied.NestedTables[assetID] = cloneWordTable(nested)
	}
	return copied
}

// clonePDFManifest deep-copies a PDF manifest for identity qualification.
func clonePDFManifest(manifest PDFManifest) PDFManifest {
	raw, err := json.Marshal(manifest)
	if err != nil {
		// PDFManifest is JSON-safe; fall back to a shallow copy on the
		// impossible error path.
		return manifest
	}
	var cloned PDFManifest
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return manifest
	}
	return cloned
}
