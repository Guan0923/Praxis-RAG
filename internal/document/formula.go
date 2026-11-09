package document

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// FormulaConverter converts one m:oMath element serialized as XML to LaTeX.
//
// The default BoundedFormulaConverter covers common OMML structures in process
// (fractions, sub/superscripts, radicals, matrices, n-ary operators, delimiters,
// accents, bars and styled text runs) and fails closed with a clear error for
// anything else. Deployments that need the full Python docxlatex coverage can
// inject an external converter here; no silent downgrade happens either way.
type FormulaConverter interface {
	ConvertOMML(oMathXML []byte) (string, error)
}

// BoundedFormulaConverter is the in-process OMML->LaTeX converter. The zero
// value is ready to use.
type BoundedFormulaConverter struct{}

// ConvertOMML implements FormulaConverter.
func (BoundedFormulaConverter) ConvertOMML(oMathXML []byte) (string, error) {
	node, err := parseXMLNode(oMathXML)
	if err != nil {
		return "", fmt.Errorf("Word formula conversion failed: %v", err)
	}
	if !isElement(node, nsMath, "oMath") {
		if isElement(node, nsMath, "oMathPara") {
			node = node.child(nsMath, "oMath")
		}
		if node == nil || !isElement(node, nsMath, "oMath") {
			return "", fmt.Errorf("Word formula conversion failed: expected an m:oMath element")
		}
	}
	if err := validateOMMLFormula(node); err != nil {
		return "", err
	}
	latex, err := convertFormulaNode(node)
	if err != nil {
		return "", fmt.Errorf("Word formula conversion failed: %v", err)
	}
	result := strings.TrimSpace(latex)
	if result == "" {
		return "", fmt.Errorf("Word formula conversion returned no text")
	}
	return result, nil
}

// ConvertOMML converts one OMML math element with the bounded converter.
func ConvertOMML(oMathXML []byte) (string, error) {
	return BoundedFormulaConverter{}.ConvertOMML(oMathXML)
}

const (
	ommlAlphabetRoman        = "roman"
	ommlAlphabetScript       = "script"
	ommlAlphabetFraktur      = "fraktur"
	ommlAlphabetDoubleStruck = "double-struck"
	ommlAlphabetSansSerif    = "sans-serif"
	ommlAlphabetMonospace    = "monospace"
)

// ommlScriptAlphabets mirrors word_reader.formula's `alphabets` map.
var ommlScriptAlphabets = map[string]string{
	ommlAlphabetRoman:        "",
	ommlAlphabetScript:       "mathcal",
	ommlAlphabetFraktur:      "mathfrak",
	ommlAlphabetDoubleStruck: "mathbb",
	ommlAlphabetSansSerif:    "mathsf",
	ommlAlphabetMonospace:    "mathtt",
}

// ommlNaryOperators maps the Word n-ary operator runes to LaTeX commands.
var ommlNaryOperators = map[rune]string{
	8719: `\prod`,
	8720: `\coprod`,
	8721: `\sum`,
	8747: `\int`,
	8748: `\iint`,
	8749: `\iiint`,
	8750: `\oint`,
	8751: `\oiint`,
	8752: `\oiiint`,
	8896: `\bigwedge`,
	8897: `\bigvee`,
	8898: `\bigcap`,
	8899: `\bigcup`,
}

// ommlWrappers are container elements accepted without their own LaTeX form.
var ommlWrappers = map[string]struct{}{
	"oMath": {}, "num": {}, "den": {}, "sub": {}, "sup": {}, "deg": {}, "fName": {}, "e": {},
}

// ommlRequiredChildren mirrors word_reader.formula's `required` map.
var ommlRequiredChildren = map[string][]string{
	"f":       {"num", "den"},
	"sSup":    {"e", "sup"},
	"sSub":    {"e", "sub"},
	"sSubSup": {"e", "sub", "sup"},
	"rad":     {"e"},
	"m":       {"mr"},
}

// ommlSupportedStructures is the bounded converter's whitelist.
var ommlSupportedStructures = map[string]struct{}{
	"oMath": {}, "r": {}, "t": {}, "f": {}, "sSup": {}, "sSub": {}, "sSubSup": {},
	"rad": {}, "m": {}, "mr": {}, "e": {}, "num": {}, "den": {}, "sub": {}, "sup": {},
	"deg": {}, "fName": {}, "nary": {}, "d": {}, "acc": {}, "bar": {},
}

// validateOMMLFormula ports the validation phase of word_reader.formula:
// unsupported operators/alphabets/structures and missing required children are
// rejected before conversion.
func validateOMMLFormula(node *xmlNode) error {
	var operatorErr error
	node.walkDescendants(func(current *xmlNode) bool {
		if operatorErr != nil {
			return false
		}
		if !isElement(current, nsMath, "chr") || !isElement(current.Parent, nsMath, "naryPr") {
			return true
		}
		value, _ := current.attrNS(nsMath, "val")
		runes := []rune(value)
		if len(runes) != 1 {
			operatorErr = fmt.Errorf("Unsupported Word formula operator: %s", value)
			return false
		}
		if _, ok := ommlNaryOperators[runes[0]]; !ok {
			operatorErr = fmt.Errorf("Unsupported Word formula operator: %s", value)
			return false
		}
		return true
	})
	if operatorErr != nil {
		return operatorErr
	}
	var alphabetErr error
	node.walkDescendants(func(current *xmlNode) bool {
		if alphabetErr != nil {
			return false
		}
		if !isElement(current, nsMath, "scr") || !isElement(current.Parent, nsMath, "rPr") {
			return true
		}
		value, _ := current.attrNS(nsMath, "val")
		if _, ok := ommlScriptAlphabets[value]; !ok {
			alphabetErr = fmt.Errorf("Unsupported styled Word formula alphabet")
			return false
		}
		return true
	})
	if alphabetErr != nil {
		return alphabetErr
	}
	var structureErr error
	var walk func(*xmlNode) bool
	walk = func(current *xmlNode) bool {
		local := current.Name.Local
		if strings.HasSuffix(local, "Pr") {
			return true
		}
		if current.Name.Space == nsMath {
			if _, ok := ommlSupportedStructures[local]; !ok {
				if _, wrapper := ommlWrappers[local]; !wrapper {
					structureErr = fmt.Errorf("Unsupported Word formula structure: %s", local)
					return false
				}
			}
			for _, needed := range ommlRequiredChildren[local] {
				if current.child(nsMath, needed) == nil {
					structureErr = fmt.Errorf("Incomplete Word formula: %s/%s", local, needed)
					return false
				}
			}
		}
		for _, child := range current.Children {
			if !walk(child) {
				return false
			}
		}
		return true
	}
	walk(node)
	return structureErr
}

// convertFormulaNode converts one validated OMML node.
func convertFormulaNode(node *xmlNode) (string, error) {
	switch node.Name.Local {
	case "oMath", "e", "num", "den", "sub", "sup", "deg", "fName":
		return convertFormulaChildren(node)
	case "mr":
		return convertMatrixRow(node)
	case "t":
		return escapeFormulaText(node.Text), nil
	case "r":
		return convertFormulaRun(node)
	case "f":
		numerator, _ := convertFormulaChild(node, "num")
		denominator, _ := convertFormulaChild(node, "den")
		return `\frac{` + numerator + `}{` + denominator + `}`, nil
	case "sSup":
		base, _ := convertFormulaChild(node, "e")
		sup, _ := convertFormulaChild(node, "sup")
		return `{` + base + `}^{` + sup + `}`, nil
	case "sSub":
		base, _ := convertFormulaChild(node, "e")
		sub, _ := convertFormulaChild(node, "sub")
		return `{` + base + `}_{` + sub + `}`, nil
	case "sSubSup":
		base, _ := convertFormulaChild(node, "e")
		sub, _ := convertFormulaChild(node, "sub")
		sup, _ := convertFormulaChild(node, "sup")
		return `{` + base + `}_{` + sub + `}^{` + sup + `}`, nil
	case "rad":
		base, _ := convertFormulaChild(node, "e")
		degree := ""
		if degHide := node.findPath(mathName("radPr"), mathName("degHide")); degHide == nil {
			degree, _ = convertFormulaChild(node, "deg")
			degree = strings.TrimSpace(degree)
		}
		if degree != "" {
			return `\sqrt[` + degree + `]{` + base + `}`, nil
		}
		return `\sqrt{` + base + `}`, nil
	case "m":
		rows := node.childrenWhere(nsMath, "mr")
		rendered := make([]string, 0, len(rows))
		for _, row := range rows {
			value, err := convertMatrixRow(row)
			if err != nil {
				return "", err
			}
			rendered = append(rendered, value)
		}
		return `\begin{matrix} ` + strings.Join(rendered, ` \\ `) + ` \end{matrix}`, nil
	case "nary":
		command := ""
		if chr := node.findPath(mathName("naryPr"), mathName("chr")); chr != nil {
			value, _ := chr.attrNS(nsMath, "val")
			if runes := []rune(value); len(runes) == 1 {
				command = ommlNaryOperators[runes[0]]
			}
		}
		if command == "" {
			command = `\int`
		}
		sub, _ := convertFormulaChild(node, "sub")
		sup, _ := convertFormulaChild(node, "sup")
		base, _ := convertFormulaChild(node, "e")
		var builder strings.Builder
		builder.WriteString(command)
		if strings.TrimSpace(sub) != "" {
			builder.WriteString(`_{` + sub + `}`)
		}
		if strings.TrimSpace(sup) != "" {
			builder.WriteString(`^{` + sup + `}`)
		}
		builder.WriteString(`{` + base + `}`)
		return builder.String(), nil
	case "d":
		begin, end := "(", ")"
		if delim := node.child(nsMath, "dPr"); delim != nil {
			if node := delim.child(nsMath, "begChr"); node != nil {
				if value, ok := node.attrNS(nsMath, "val"); ok {
					begin = value
				}
			}
			if node := delim.child(nsMath, "endChr"); node != nil {
				if value, ok := node.attrNS(nsMath, "val"); ok {
					end = value
				}
			}
		}
		if begin == "" {
			begin = "."
		}
		if end == "" {
			end = "."
		}
		content, err := convertFormulaChildren(node)
		if err != nil {
			return "", err
		}
		return `\left` + begin + " " + content + ` \right` + end, nil
	case "acc":
		content, _ := convertFormulaChild(node, "e")
		command := `\hat`
		if accent := node.findPath(mathName("accPr"), mathName("chr")); accent != nil {
			value, _ := accent.attrNS(nsMath, "val")
			if mapped, ok := ommlAccents[value]; ok {
				command = mapped
			} else if value != "" {
				return "", fmt.Errorf("Unsupported Word formula accent: %s", value)
			}
		}
		return command + `{` + content + `}`, nil
	case "bar":
		content, _ := convertFormulaChild(node, "e")
		command := `\overline`
		if position := node.findPath(mathName("barPr"), mathName("pos")); position != nil {
			if value, _ := position.attrNS(nsMath, "val"); value == "bot" {
				command = `\underline`
			}
		}
		return command + `{` + content + `}`, nil
	}
	return "", fmt.Errorf("Unsupported Word formula structure: %s", node.Name.Local)
}

// ommlAccents maps combining accent characters to LaTeX commands.
var ommlAccents = map[string]string{
	"\u0302": `\hat`,
	"\u0303": `\tilde`,
	"\u0304": `\bar`,
	"\u0305": `\overline`,
	"\u20d7": `\vec`,
	"\u0307": `\dot`,
	"\u0308": `\ddot`,
	"\u0301": `\acute`,
	"\u0300": `\grave`,
	"\u030c": `\check`,
	"\u0306": `\breve`,
}

func convertFormulaChildren(node *xmlNode) (string, error) {
	var builder strings.Builder
	for _, child := range node.Children {
		value, err := convertFormulaNode(child)
		if err != nil {
			return "", err
		}
		builder.WriteString(value)
	}
	return builder.String(), nil
}

func convertFormulaChild(node *xmlNode, local string) (string, error) {
	child := node.child(nsMath, local)
	if child == nil {
		return "", nil
	}
	return convertFormulaChildren(child)
}

func convertMatrixRow(node *xmlNode) (string, error) {
	cells := make([]string, 0, len(node.Children))
	for _, child := range node.Children {
		if !isElement(child, nsMath, "e") {
			continue
		}
		value, err := convertFormulaChildren(child)
		if err != nil {
			return "", err
		}
		cells = append(cells, value)
	}
	return strings.Join(cells, " & "), nil
}

// convertFormulaRun ports the styled_run override in word_reader.formula.
func convertFormulaRun(node *xmlNode) (string, error) {
	var builder strings.Builder
	for _, child := range node.Children {
		if isElement(child, nsMath, "rPr") {
			continue
		}
		value, err := convertFormulaNode(child)
		if err != nil {
			return "", err
		}
		builder.WriteString(value)
	}
	value := builder.String()
	rPr := node.child(nsMath, "rPr")
	if rPr == nil {
		return value, nil
	}
	alphabet := ""
	if script := rPr.child(nsMath, "scr"); script != nil {
		raw, _ := script.attrNS(nsMath, "val")
		alphabet = ommlScriptAlphabets[raw]
	}
	if alphabet != "" {
		value = `\` + alphabet + `{` + value + `}`
	}
	command := ""
	if style := rPr.child(nsMath, "sty"); style != nil {
		raw, _ := style.attrNS(nsMath, "val")
		switch raw {
		case "b":
			command = "mathbf"
		case "bi":
			command = "boldsymbol"
		case "p":
			command = "mathrm"
		}
	}
	if command != "" && !(alphabet != "" && command == "mathrm") {
		value = `\` + command + `{` + value + `}`
	}
	return value, nil
}

// escapeFormulaText escapes LaTeX special characters in formula text runs.
func escapeFormulaText(text string) string {
	var builder strings.Builder
	builder.Grow(len(text) + 8)
	for _, value := range text {
		switch value {
		case '\\', '{', '}', '_', '^', '%', '&', '#', '$':
			builder.WriteByte('\\')
		}
		builder.WriteRune(value)
	}
	return builder.String()
}

// ommlPrefixes maps OOXML namespaces to the prefixes used when re-serializing a
// formula subtree for the converter interface.
var ommlPrefixes = []struct {
	uri    string
	prefix string
}{
	{nsMath, "m"},
	{nsWord, "w"},
	{nsRelationships, "r"},
	{nsDrawingML, "a"},
	{nsVML, "v"},
	{nsWordDrawing, "wp"},
	{nsMarkupCompat, "mc"},
	{nsContentTypes, "ct"},
}

func ommlPrefixFor(uri string) (string, bool) {
	for _, entry := range ommlPrefixes {
		if entry.uri == uri {
			return entry.prefix, true
		}
	}
	return "", false
}

// serializeOMML renders one xmlNode subtree back to XML so external
// FormulaConverter implementations receive a self-contained OMML fragment.
func serializeOMML(node *xmlNode) ([]byte, error) {
	var builder bytes.Buffer
	prefix, ok := ommlPrefixFor(node.Name.Space)
	if !ok {
		return nil, fmt.Errorf("cannot serialize Word formula namespace %q", node.Name.Space)
	}
	builder.WriteString("<" + prefix + ":" + node.Name.Local)
	for _, entry := range ommlPrefixes {
		builder.WriteString(" xmlns:" + entry.prefix + `="` + entry.uri + `"`)
	}
	builder.WriteString(">")
	for _, child := range node.Children {
		if err := writeOMMLNode(&builder, child); err != nil {
			return nil, err
		}
	}
	builder.WriteString("</" + prefix + ":" + node.Name.Local + ">")
	return builder.Bytes(), nil
}

func writeOMMLNode(builder *bytes.Buffer, node *xmlNode) error {
	prefix, known := ommlPrefixFor(node.Name.Space)
	if node.Name.Space != "" && !known {
		return fmt.Errorf("cannot serialize Word formula namespace %q", node.Name.Space)
	}
	name := node.Name.Local
	if prefix != "" {
		name = prefix + ":" + name
	}
	builder.WriteString("<" + name)
	for _, attr := range node.Attrs {
		if attr.Name.Local == "xmlns" || attr.Name.Space == "xmlns" {
			continue
		}
		attrName := attr.Name.Local
		if attr.Name.Space != "" {
			attrPrefix, ok := ommlPrefixFor(attr.Name.Space)
			if !ok {
				return fmt.Errorf("cannot serialize Word formula attribute namespace %q", attr.Name.Space)
			}
			attrName = attrPrefix + ":" + attrName
		}
		builder.WriteString(" " + attrName + `="`)
		if err := xml.EscapeText(builder, []byte(attr.Value)); err != nil {
			return err
		}
		builder.WriteString(`"`)
	}
	builder.WriteString(">")
	if node.Text != "" {
		if err := xml.EscapeText(builder, []byte(node.Text)); err != nil {
			return err
		}
	}
	for _, child := range node.Children {
		if err := writeOMMLNode(builder, child); err != nil {
			return err
		}
	}
	builder.WriteString("</" + name + ">")
	return nil
}
