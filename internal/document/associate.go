package document

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// captionPrefix mirrors the prefix part of word_reader.CAPTION: it captures the
// object kind (图/表/figure/fig/table) and the number. The Python pattern ends
// with r"(?:\b|(?=[\s:：、.．]))"; Go's regexp has no lookaround and its \b is
// ASCII-only, so the trailing boundary is checked by pythonWordBoundaryAfter.
var captionPrefix = regexp.MustCompile(`(?i)^\s*(图|表|figure|fig\.?|table)\s*(\d+(?:[.\-]\d+)*|[一二三四五六七八九十]+)`)

// noteLine mirrors word_reader.NOTE.
var noteLine = regexp.MustCompile(`(?i)^\s*(?:注|说明|来源|资料来源|note|notes|source)\s*[:：.]`)

// captionParts is one successful word_reader.CAPTION.match.
type captionParts struct {
	Kind   string
	Number string
}

// parseCaption ports word_reader.CAPTION.match.
func parseCaption(text string) *captionParts {
	match := captionPrefix.FindStringSubmatchIndex(text)
	if match == nil {
		return nil
	}
	rest := text[match[1]:]
	if rest != "" {
		value, _ := utf8.DecodeRuneInString(rest)
		if isPythonWordRune(value) {
			return nil
		}
	}
	return &captionParts{Kind: text[match[2]:match[3]], Number: text[match[4]:match[5]]}
}

// captionIsTable reports whether a caption kind names a table, mirroring the
// Python `match["kind"].lower() in {"table", "表"}` checks.
func captionIsTable(kind string) bool {
	switch strings.ToLower(kind) {
	case "table", "表":
		return true
	}
	return false
}

// isPythonWordRune reports whether rune counts as \w for Python's re module.
func isPythonWordRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value) ||
		unicode.Is(unicode.Nl, value) || unicode.Is(unicode.No, value)
}

// styleMentionsCaption mirrors `any("caption" in s.lower() for s in styles)`.
func styleMentionsCaption(styles []string) bool {
	for _, style := range styles {
		if strings.Contains(strings.ToLower(style), "caption") {
			return true
		}
	}
	return false
}

// associate ports word_reader.WordReader.associate: captions/notes are attached
// to the nearest uniquely identifiable picture/table, ambiguous captions skip
// their affected objects (including nested descendants) with a diagnostic, and
// the remaining manifest is returned in order.
func (r *wordReader) associate() (int, error) {
	assets := make(map[string]*WordManifestAsset, len(r.assets))
	for _, asset := range r.assets {
		assets[asset.AssetID] = asset
	}
	skipped := make(map[string]struct{})
	for index := 0; index < len(r.blocks); index++ {
		block := &r.blocks[index]
		if block.Kind != KindCaption && block.Kind != KindNote {
			continue
		}
		location := block.Location
		same := make([]*WordManifestAsset, 0, len(r.assets))
		for _, asset := range r.assets {
			if asset.Location.Part == location.Part && asset.Location.Scope == location.Scope &&
				asset.Location.Section == location.Section {
				same = append(same, asset)
			}
		}
		match := parseCaption(block.Text)
		targetTable := false
		if match != nil {
			targetTable = captionIsTable(match.Kind)
			kind := "picture"
			if targetTable {
				kind = "table"
			}
			filtered := make([]*WordManifestAsset, 0, len(same))
			for _, asset := range same {
				if asset.Kind == kind {
					filtered = append(filtered, asset)
				}
			}
			same = filtered
		}
		explicit := make([]*WordManifestAsset, 0, len(same))
		for _, asset := range same {
			if intersectsStrings(asset.Bookmarks, block.References) {
				explicit = append(explicit, asset)
			}
		}
		if block.Kind == KindNote && index > 0 {
			previous := r.blocks[index-1]
			if previous.Kind == KindCaption || previous.Kind == KindNote {
				explicit = explicit[:0]
				for _, asset := range same {
					if containsString(previous.AssociatedAssets, asset.AssetID) {
						explicit = append(explicit, asset)
					}
				}
			}
		}
		if match != nil {
			for _, asset := range same {
				for _, label := range asset.Labels {
					labelMatch := parseCaption(label)
					if labelMatch == nil || labelMatch.Number != match.Number ||
						captionIsTable(labelMatch.Kind) != targetTable {
						continue
					}
					if !containsAsset(explicit, asset) {
						explicit = append(explicit, asset)
					}
				}
			}
			numbered := make([]string, 0, len(r.blocks))
			for position := range r.blocks {
				other := &r.blocks[position]
				if other.Kind != KindCaption {
					continue
				}
				if other.Location.Part != location.Part || other.Location.Scope != location.Scope ||
					other.Location.Section != location.Section {
					continue
				}
				otherMatch := parseCaption(other.Text)
				if otherMatch == nil || captionIsTable(otherMatch.Kind) != targetTable {
					continue
				}
				numbered = append(numbered, otherMatch.Number)
			}
			if len(explicit) == 0 && len(numbered) == len(same) && sequentialNumbers(numbered, len(same)) {
				position, err := strconv.Atoi(match.Number)
				if err != nil {
					return 0, fmt.Errorf("invalid caption number %q: %v", match.Number, err)
				}
				if position >= 1 && position <= len(same) {
					explicit = append(explicit, same[position-1])
				}
			}
		}
		adjacent := make([]*WordManifestAsset, 0, 2)
		for _, direction := range []int{-1, 1} {
			cursor := index + direction
			for cursor >= 0 && cursor < len(r.blocks) {
				neighbor := &r.blocks[cursor]
				if neighbor.Location.Section != location.Section || neighbor.Location.Part != location.Part {
					break
				}
				if neighbor.Location.Scope != location.Scope {
					if strings.HasPrefix(neighbor.Location.Scope, location.Scope+"/") {
						cursor += direction
						continue
					}
					break
				}
				if neighbor.AssetID != "" {
					asset, ok := assets[neighbor.AssetID]
					if !ok {
						break
					}
					if containsAsset(same, asset) {
						adjacent = append(adjacent, asset)
						cursor += direction
						continue
					}
					break
				}
				if neighbor.Kind != KindCaption && neighbor.Kind != KindNote {
					break
				}
				cursor += direction
			}
		}
		candidates := explicit
		if len(candidates) == 0 {
			candidates = adjacent
		}
		if block.Kind == KindNote {
			filtered := make([]*WordManifestAsset, 0, len(candidates))
			blockOrder := block.Order
			for _, asset := range candidates {
				if assetBlockOrder(r.blocks, asset.AssetID) < blockOrder {
					filtered = append(filtered, asset)
				}
			}
			candidates = filtered
		}
		unique := make([]*WordManifestAsset, 0, len(candidates))
		seen := make(map[string]struct{}, len(candidates))
		for _, asset := range candidates {
			if _, ok := seen[asset.AssetID]; ok {
				continue
			}
			seen[asset.AssetID] = struct{}{}
			unique = append(unique, asset)
		}
		switch {
		case len(unique) == 1:
			asset := unique[0]
			annotation := WordAnnotation{Text: block.Text, Ref: block.Ref, Location: location}
			if block.Kind == KindNote {
				asset.Footnotes = append(asset.Footnotes, annotation)
				if asset.Table != nil {
					asset.Table.Footnotes = append(asset.Table.Footnotes, block.Text)
				}
			} else {
				asset.Captions = append(asset.Captions, annotation)
				captionTexts := make([]string, len(asset.Captions))
				for index, caption := range asset.Captions {
					captionTexts[index] = caption.Text
				}
				asset.Caption = strings.Join(captionTexts, "\n")
				asset.CaptionSource = "document"
			}
			block.AssociatedAssets = append(block.AssociatedAssets, asset.AssetID)
		case block.Kind == KindCaption:
			affected := unique
			if len(affected) == 0 {
				affected = same
			}
			ids := make([]string, 0, len(affected))
			for _, asset := range affected {
				skipped[asset.AssetID] = struct{}{}
				ids = append(ids, asset.AssetID)
			}
			r.diagnostics = append(r.diagnostics, WordDiagnostic{
				Kind:     "unassociated_caption",
				Message:  "Caption could not be uniquely associated; affected objects were skipped.",
				Location: location,
				Text:     block.Text,
				AssetIDs: ids,
			})
		}
	}
	// Descendants of a skipped outer table must not remain independently indexed.
	for changed := true; changed; {
		previous := len(skipped)
		for _, asset := range r.assets {
			for item := range skipped {
				if strings.Contains(asset.Location.Scope, "/"+item+"/") {
					skipped[asset.AssetID] = struct{}{}
				}
			}
		}
		changed = len(skipped) != previous
	}
	for _, asset := range r.assets {
		if _, ok := skipped[asset.AssetID]; ok && asset.ImageFile != nil {
			delete(r.images, asset.AssetID)
		}
	}
	prunedAssets := make([]*WordManifestAsset, 0, len(r.assets))
	for _, asset := range r.assets {
		if _, ok := skipped[asset.AssetID]; !ok {
			prunedAssets = append(prunedAssets, asset)
		}
	}
	r.assets = prunedAssets
	prunedBlocks := make([]WordManifestBlock, 0, len(r.blocks))
	for _, block := range r.blocks {
		if block.AssetID != "" {
			if _, ok := skipped[block.AssetID]; ok {
				continue
			}
		}
		if len(block.AssociatedAssets) > 0 {
			kept := make([]string, 0, len(block.AssociatedAssets))
			for _, assetID := range block.AssociatedAssets {
				if _, ok := skipped[assetID]; !ok {
					kept = append(kept, assetID)
				}
			}
			block.AssociatedAssets = kept
		}
		prunedBlocks = append(prunedBlocks, block)
	}
	r.blocks = prunedBlocks
	return len(skipped), nil
}

func intersectsStrings(left, right []string) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	lookup := make(map[string]struct{}, len(right))
	for _, value := range right {
		lookup[value] = struct{}{}
	}
	for _, value := range left {
		if _, ok := lookup[value]; ok {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsAsset(assets []*WordManifestAsset, target *WordManifestAsset) bool {
	for _, asset := range assets {
		if asset.AssetID == target.AssetID {
			return true
		}
	}
	return false
}

// sequentialNumbers reports whether numbers is exactly "1".."count".
func sequentialNumbers(numbers []string, count int) bool {
	if len(numbers) != count {
		return false
	}
	seen := make(map[string]struct{}, count)
	for _, number := range numbers {
		seen[number] = struct{}{}
	}
	if len(seen) != count {
		return false
	}
	for position := 1; position <= count; position++ {
		if _, ok := seen[strconv.Itoa(position)]; !ok {
			return false
		}
	}
	return true
}

// assetBlockOrder is the manifest block order of the block that owns assetID.
func assetBlockOrder(blocks []WordManifestBlock, assetID string) int {
	for _, block := range blocks {
		if block.AssetID == assetID {
			return block.Order
		}
	}
	return -1
}
