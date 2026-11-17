package document

// This file adds the Word-equivalent caption/note association to Docling
// manifests. Docling preserves explicit references (for example the
// <figcaption> of an HTML figure, or a table <caption>) but its DOCX backend
// keeps caption paragraphs as ordinary text. The service reports the ordered
// blocks and explicit associations; the conservative association below only
// attaches a caption when exactly one picture/table can own it, so an
// unlabelled caption stays searchable text instead of being forced onto a
// neighbouring object.

// AssociateDoclingCaptions classifies caption and note paragraphs of a Docling
// manifest and attaches the unambiguous ones to the adjacent picture/table.
// The manifest is returned with the same block order; associated caption
// blocks stay in the manifest with associated_assets set, which the Word chunk
// builder already skips (their text is searched through the asset chunk).
func AssociateDoclingCaptions(manifest WordManifest) WordManifest {
	assets := make(map[string]*WordManifestAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if asset != nil {
			assets[asset.AssetID] = asset
		}
	}
	// Classify plain paragraphs that are caption/note shaped. Explicit Docling
	// captions already carry the caption kind and an associated asset.
	for index := range manifest.Blocks {
		block := &manifest.Blocks[index]
		if block.AssetID != "" || len(block.AssociatedAssets) > 0 {
			continue
		}
		if block.Kind == KindCaption || block.Kind == KindNote {
			continue
		}
		if parseCaption(block.Text) != nil {
			block.Kind = KindCaption
			continue
		}
		if noteLine.MatchString(block.Text) {
			block.Kind = KindNote
		}
	}
	for index := range manifest.Blocks {
		block := &manifest.Blocks[index]
		if block.Kind != KindCaption && block.Kind != KindNote {
			continue
		}
		if block.AssetID != "" || len(block.AssociatedAssets) > 0 {
			continue
		}
		candidate := doclingCaptionCandidate(manifest.Blocks, assets, index)
		if candidate == nil {
			manifest.Diagnostics = append(manifest.Diagnostics, WordDiagnostic{
				Kind:     "unassociated_caption",
				Message:  "Caption could not be uniquely associated; it is kept as text.",
				Location: block.Location,
				Text:     block.Text,
			})
			continue
		}
		annotation := WordAnnotation{Text: block.Text, Ref: block.Ref, Location: block.Location}
		if block.Kind == KindNote {
			candidate.Footnotes = append(candidate.Footnotes, annotation)
			if candidate.Table != nil {
				candidate.Table.Footnotes = append(candidate.Table.Footnotes, block.Text)
			}
		} else {
			candidate.Captions = append(candidate.Captions, annotation)
			captionTexts := make([]string, len(candidate.Captions))
			for position, caption := range candidate.Captions {
				captionTexts[position] = caption.Text
			}
			candidate.Caption = joinCaptionTexts(captionTexts)
			candidate.CaptionSource = "document"
		}
		block.AssociatedAssets = append(block.AssociatedAssets, candidate.AssetID)
	}
	if manifest.Diagnostics == nil {
		manifest.Diagnostics = []WordDiagnostic{}
	}
	return manifest
}

// doclingCaptionCandidate returns the single picture/table a caption may
// belong to, or nil when ownership is ambiguous.
func doclingCaptionCandidate(blocks []WordManifestBlock, assets map[string]*WordManifestAsset,
	captionIndex int) *WordManifestAsset {
	caption := &blocks[captionIndex]
	targetKind := "picture"
	var numbered bool
	var number string
	if match := parseCaption(caption.Text); match != nil {
		number = match.Number
		numbered = true
		if captionIsTable(match.Kind) {
			targetKind = "table"
		}
	}
	if !numbered {
		// A note belongs to a preceding asset only.
		return doclingAdjacentAsset(blocks, assets, captionIndex, -1, "")
	}
	same := make([]*WordManifestAsset, 0, len(assets))
	for _, asset := range assets {
		if asset.Location.Part != caption.Location.Part ||
			asset.Location.Scope != caption.Location.Scope ||
			asset.Location.Section != caption.Location.Section {
			continue
		}
		if asset.Kind == targetKind {
			same = append(same, asset)
		}
	}
	// Numbered captions that are sequential over every object of the same kind
	// map by position, mirroring the native Word association.
	numbers := make([]string, 0, len(blocks))
	for position := range blocks {
		candidate := &blocks[position]
		if candidate.Kind != KindCaption || candidate.Location.Part != caption.Location.Part ||
			candidate.Location.Scope != caption.Location.Scope ||
			candidate.Location.Section != caption.Location.Section {
			continue
		}
		match := parseCaption(candidate.Text)
		if match == nil || captionIsTable(match.Kind) != (targetKind == "table") {
			continue
		}
		numbers = append(numbers, match.Number)
	}
	if len(same) > 0 && len(numbers) == len(same) && sequentialNumbers(numbers, len(same)) {
		if position := captionNumberPosition(number); position >= 1 && position <= len(same) {
			return same[position-1]
		}
	}
	// Otherwise only a unique adjacent asset of the matching kind qualifies.
	for _, direction := range []int{-1, 1} {
		if asset := doclingAdjacentAsset(blocks, assets, captionIndex, direction, targetKind); asset != nil {
			return asset
		}
	}
	return nil
}

// doclingAdjacentAsset walks in one direction from a caption block and returns
// the single adjacent asset of kind (empty kind accepts picture or table).
// A second adjacent asset or any ordinary paragraph makes the result
// ambiguous.
func doclingAdjacentAsset(blocks []WordManifestBlock, assets map[string]*WordManifestAsset,
	captionIndex, direction int, kind string) *WordManifestAsset {
	caption := &blocks[captionIndex]
	var found *WordManifestAsset
	for cursor := captionIndex + direction; cursor >= 0 && cursor < len(blocks); cursor += direction {
		block := &blocks[cursor]
		if block.Location.Part != caption.Location.Part ||
			block.Location.Scope != caption.Location.Scope ||
			block.Location.Section != caption.Location.Section {
			break
		}
		if block.Kind == KindCaption || block.Kind == KindNote {
			continue
		}
		if block.AssetID == "" {
			break
		}
		asset, ok := assets[block.AssetID]
		if !ok {
			break
		}
		if kind != "" && asset.Kind != kind {
			break
		}
		if found != nil {
			return nil
		}
		found = asset
	}
	return found
}

// captionNumberPosition parses an integer caption number; "1.2" or Chinese
// numerals have no positional meaning and return 0.
func captionNumberPosition(number string) int {
	position := 0
	for _, character := range number {
		if character < '0' || character > '9' {
			return 0
		}
		position = position*10 + int(character-'0')
	}
	return position
}

// joinCaptionTexts joins captured caption paragraphs, matching the native Word
// association's "\n" join.
func joinCaptionTexts(captions []string) string {
	result := ""
	for index, caption := range captions {
		if index > 0 {
			result += "\n"
		}
		result += caption
	}
	return result
}
