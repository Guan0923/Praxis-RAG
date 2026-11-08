package document

import (
	"regexp"
	"strings"
)

// markdownBlock is the subset of markdown-it top-level token behaviour that
// document_readers._markdown_blocks relies on: ordered top-level blocks with
// their raw line ranges, heading kinds and the accumulated heading path.
//
// The Python implementation parses with markdown_it.MarkdownIt("commonmark")
// and then walks level-0 block tokens. Go has no stdlib Markdown parser and the
// migration forbids new dependencies for this package, so this file implements
// a CommonMark-conscious block scanner covering the constructs that affect
// extracted text and heading paths: ATX/setext headings, fenced and indented
// code, thematic breaks, block quotes, lists, HTML blocks and paragraphs.
// Inline syntax is intentionally not parsed: Python keeps raw block source
// lines, and only heading titles need the inline content string.
var (
	markdownATX        = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*))?[ \t]*$`)
	markdownATXClosing = regexp.MustCompile(`(?:^|[ \t])#+[ \t]*$`)
	markdownFence      = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	markdownSetext     = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	markdownList       = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])(?:[ \t]+|$)`)
	markdownHTML       = regexp.MustCompile(`^ {0,3}<(?:[A-Za-z][A-Za-z0-9-]*|[!/?])`)
)

type markdownHeading struct {
	level int
	title string
}

// markdownBlocks returns ordered blocks for one Markdown document.
func markdownBlocks(text string) []Block {
	lines := splitLinesKeepEnds(text)
	headings := make([]markdownHeading, 0, 8)
	blocks := make([]Block, 0, len(lines))
	cursor := 0
	index := 0
	for index < len(lines) {
		if strings.TrimSpace(stripLineEnding(lines[index])) == "" {
			index++
			continue
		}
		start := index
		line := stripLineEnding(lines[index])
		end := index + 1
		kind := KindParagraph
		level := 0
		title := ""
		switch {
		case atxLevel(line) > 0:
			level, title, _ = atxHeading(line)
			kind = KindHeading
		case fenceStart(line):
			end, kind = fencedBlockEnd(lines, index), KindCode
		case thematicBreak(line):
			// A level-0 "hr" token: document_readers emits it as a paragraph.
		case blockquoteStart(line):
			end = blockquoteBlockEnd(lines, index)
		case markdownList.MatchString(line):
			end = listBlockEnd(lines, index)
		case markdownHTML.MatchString(line):
			end = htmlBlockEnd(lines, index)
		case indentedCodeLine(lines[index]):
			end, kind = indentedCodeBlockEnd(lines, index), KindCode
		default:
			end, kind, level, title = paragraphBlockEnd(lines, index)
		}
		if start > cursor {
			gap := strings.Join(lines[cursor:start], "")
			if strings.TrimSpace(gap) != "" {
				blocks = append(blocks, Block{Text: strings.TrimSpace(gap), Kind: KindParagraph, Headings: headingTitles(headings)})
			}
		}
		if kind == KindHeading {
			headings = updateHeadings(headings, level, title)
		}
		raw := strings.TrimRight(strings.Join(lines[start:end], ""), "\r\n")
		blocks = append(blocks, Block{Text: raw, Kind: kind, Headings: headingTitles(headings)})
		cursor = end
		index = end
	}
	if cursor < len(lines) {
		trailing := strings.Join(lines[cursor:], "")
		if strings.TrimSpace(trailing) != "" {
			blocks = append(blocks, Block{
				Text:     strings.TrimRight(trailing, "\r\n"),
				Kind:     KindParagraph,
				Headings: headingTitles(headings),
			})
		}
	}
	return blocks
}

func splitLinesKeepEnds(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func stripLineEnding(line string) string {
	return strings.TrimRight(line, "\r\n")
}

func atxLevel(line string) int {
	match := markdownATX.FindStringSubmatch(line)
	if match == nil {
		return 0
	}
	return len(match[1])
}

// atxHeading returns the heading level and inline content title of one ATX
// heading line.
func atxHeading(line string) (level int, title string, ok bool) {
	match := markdownATX.FindStringSubmatch(line)
	if match == nil {
		return 0, "", false
	}
	level = len(match[1])
	title = match[2]
	if closing := markdownATXClosing.FindStringIndex(title); closing != nil {
		title = title[:closing[0]]
	}
	return level, strings.TrimSpace(title), true
}

func fenceStart(line string) bool {
	return markdownFence.MatchString(line)
}

// fencedBlockEnd returns the exclusive end line index of a fenced code block,
// including the closing fence when present.
func fencedBlockEnd(lines []string, start int) int {
	opener := markdownFence.FindStringSubmatch(stripLineEnding(lines[start]))
	if opener == nil {
		return start + 1
	}
	marker := opener[1][0]
	count := len(opener[1])
	for index := start + 1; index < len(lines); index++ {
		line := stripLineEnding(lines[index])
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 {
			continue
		}
		run := 0
		for run < len(trimmed) && trimmed[run] == marker {
			run++
		}
		if run < count {
			continue
		}
		if strings.TrimSpace(trimmed[run:]) == "" {
			return index + 1
		}
	}
	return len(lines)
}

func thematicBreak(line string) bool {
	spaces := 0
	for spaces < len(line) && spaces < 4 && line[spaces] == ' ' {
		spaces++
	}
	if spaces > 3 {
		return false
	}
	rest := line[spaces:]
	if rest == "" {
		return false
	}
	var marker byte
	count := 0
	for index := 0; index < len(rest); index++ {
		char := rest[index]
		if char == ' ' || char == '\t' {
			continue
		}
		if marker == 0 {
			marker = char
		} else if char != marker {
			return false
		}
		count++
	}
	if count < 3 {
		return false
	}
	return marker == '*' || marker == '-' || marker == '_'
}

func blockquoteStart(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	return strings.HasPrefix(trimmed, ">")
}

// blockquoteBlockEnd consumes a block quote including lazy continuations and
// interior blank lines that are followed by more quoted lines.
func blockquoteBlockEnd(lines []string, start int) int {
	end := start + 1
	cursor := start + 1
	for cursor < len(lines) {
		line := stripLineEnding(lines[cursor])
		if blockquoteStart(line) {
			cursor++
			end = cursor
			continue
		}
		if strings.TrimSpace(line) == "" {
			next := cursor
			for next < len(lines) && strings.TrimSpace(stripLineEnding(lines[next])) == "" {
				next++
			}
			if next < len(lines) && blockquoteStart(stripLineEnding(lines[next])) {
				cursor = next
				continue
			}
			return end
		}
		// Lazy continuation lines belong to the quote when directly adjacent.
		if cursor == end {
			cursor++
			end = cursor
			continue
		}
		return end
	}
	return end
}

// listBlockEnd consumes a top-level list block. Python treats the whole list
// token as one paragraph block of raw Markdown, so only the block range matters.
func listBlockEnd(lines []string, start int) int {
	end := start + 1
	cursor := start + 1
	for cursor < len(lines) {
		line := stripLineEnding(lines[cursor])
		if strings.TrimSpace(line) == "" {
			cursor++
			continue
		}
		if markdownList.MatchString(line) || leadingIndent(line) >= 2 || indentedCodeLine(line) {
			cursor++
			end = cursor
			continue
		}
		break
	}
	return end
}

// htmlBlockEnd consumes an HTML block. CommonMark ends most HTML blocks at a
// blank line, which is also what matters for raw block text.
func htmlBlockEnd(lines []string, start int) int {
	end := start + 1
	for end < len(lines) && strings.TrimSpace(stripLineEnding(lines[end])) != "" {
		end++
	}
	return end
}

func indentedCodeLine(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

func indentedCodeBlockEnd(lines []string, start int) int {
	end := start + 1
	cursor := start + 1
	for cursor < len(lines) {
		line := stripLineEnding(lines[cursor])
		if strings.TrimSpace(line) == "" || indentedCodeLine(line) {
			cursor++
			if strings.TrimSpace(line) != "" {
				end = cursor
			}
			continue
		}
		break
	}
	return end
}

func leadingIndent(line string) int {
	width := 0
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case ' ':
			width++
		case '\t':
			width += 4
		default:
			return width
		}
	}
	return width
}

func setextLevel(line string) (level int, ok bool) {
	match := markdownSetext.FindStringSubmatch(line)
	if match == nil {
		return 0, false
	}
	if match[1][0] == '=' {
		return 1, true
	}
	return 2, true
}

// interruptsParagraph reports whether a line starts a block that may interrupt
// a paragraph (CommonMark "paragraph interruption" rules).
func interruptsParagraph(line string) bool {
	switch {
	case atxLevel(line) > 0:
		return true
	case fenceStart(line):
		return true
	case thematicBreak(line):
		return true
	case blockquoteStart(line):
		return true
	case markdownList.MatchString(line):
		return true
	case markdownHTML.MatchString(line):
		return true
	}
	return false
}

// paragraphBlockEnd consumes a paragraph, converting it into a setext heading
// when the following line is a setext underline.
func paragraphBlockEnd(lines []string, start int) (end int, kind string, level int, title string) {
	collected := make([]string, 0, 4)
	index := start
	for index < len(lines) {
		line := stripLineEnding(lines[index])
		if strings.TrimSpace(line) == "" {
			break
		}
		if index > start {
			if underlineLevel, ok := setextLevel(line); ok {
				return index + 1, KindHeading, underlineLevel, strings.TrimSpace(strings.Join(collected, "\n"))
			}
			if interruptsParagraph(line) {
				break
			}
		}
		collected = append(collected, line)
		index++
		if index < len(lines) {
			if underlineLevel, ok := setextLevel(stripLineEnding(lines[index])); ok {
				return index + 1, KindHeading, underlineLevel, strings.TrimSpace(strings.Join(collected, "\n"))
			}
		}
	}
	return index, KindParagraph, 0, ""
}

func updateHeadings(headings []markdownHeading, level int, title string) []markdownHeading {
	next := make([]markdownHeading, 0, level)
	for _, heading := range headings {
		if heading.level < level {
			next = append(next, heading)
		}
	}
	return append(next, markdownHeading{level: level, title: title})
}

func headingTitles(headings []markdownHeading) []string {
	if len(headings) == 0 {
		return nil
	}
	titles := make([]string, len(headings))
	for index, heading := range headings {
		titles[index] = heading.title
	}
	return titles
}
