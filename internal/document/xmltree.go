package document

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// OOXML namespace URIs used by word_reader.py.
const (
	nsWord          = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsMath          = "http://schemas.openxmlformats.org/officeDocument/2006/math"
	nsRelationships = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsDrawingML     = "http://schemas.openxmlformats.org/drawingml/2006/main"
	nsVML           = "urn:schemas-microsoft-com:vml"
	nsWordDrawing   = "http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"
	nsMarkupCompat  = "http://schemas.openxmlformats.org/markup-compatibility/2006"
	nsContentTypes  = "http://schemas.openxmlformats.org/package/2006/content-types"
)

// xmlNode is a minimal DOM node: element name, attributes, direct text and
// ordered children. It is enough to reproduce the lxml element access used by
// word_reader.py (find/findall/iter/get/text/iterancestors) with encoding/xml,
// which avoids a third-party XML dependency.
type xmlNode struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Children []*xmlNode
	Text     string
	// Parent supports iterancestors-style lookups (picture labels/bookmarks).
	Parent *xmlNode
}

// parseXMLNode parses one XML document. External entity resolution and DTD
// loading are never performed, matching lxml's
// XMLParser(resolve_entities=False, no_network=True, load_dtd=False).
func parseXMLNode(data []byte) (*xmlNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	root := &xmlNode{}
	stack := []*xmlNode{root}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			node := &xmlNode{Name: element.Name, Attrs: element.Attr}
			parent := stack[len(stack)-1]
			parent.Children = append(parent.Children, node)
			if parent != root {
				node.Parent = parent
			} else {
				node.Parent = nil
			}
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) <= 1 {
				return nil, fmt.Errorf("unbalanced XML element")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			current := stack[len(stack)-1]
			current.Text += string(element)
		}
	}
	if len(root.Children) == 0 {
		return nil, fmt.Errorf("empty XML document")
	}
	return root.Children[0], nil
}

// isElement reports whether node is the namespaced element ns:local.
func isElement(node *xmlNode, namespace, local string) bool {
	if node == nil {
		return false
	}
	return node.Name.Space == namespace && node.Name.Local == local
}

// child returns the first direct child matching ns:local.
func (n *xmlNode) child(namespace, local string) *xmlNode {
	if n == nil {
		return nil
	}
	for _, candidate := range n.Children {
		if isElement(candidate, namespace, local) {
			return candidate
		}
	}
	return nil
}

// findPath walks a direct-child path such as w:pPr/w:pStyle.
func (n *xmlNode) findPath(path ...xmlName) *xmlNode {
	current := n
	for _, step := range path {
		current = current.child(step.namespace, step.local)
		if current == nil {
			return nil
		}
	}
	return current
}

// childrenWhere returns all direct children matching ns:local.
func (n *xmlNode) childrenWhere(namespace, local string) []*xmlNode {
	if n == nil {
		return nil
	}
	result := make([]*xmlNode, 0, len(n.Children))
	for _, candidate := range n.Children {
		if isElement(candidate, namespace, local) {
			result = append(result, candidate)
		}
	}
	return result
}

// wordAttr returns a WordprocessingML attribute value. Attributes produced by
// Word are namespace-qualified (w:val); a lenient local-name fallback keeps
// hand-built or third-party packages readable.
func (n *xmlNode) wordAttr(local string) (string, bool) {
	if n == nil {
		return "", false
	}
	for _, attr := range n.Attrs {
		if attr.Name.Space == nsWord && attr.Name.Local == local {
			return attr.Value, true
		}
	}
	for _, attr := range n.Attrs {
		if attr.Name.Local == local && attr.Name.Space == "" {
			return attr.Value, true
		}
	}
	return "", false
}

// attrNS returns an attribute value with an exact namespace ("" for unqualified
// attributes).
func (n *xmlNode) attrNS(namespace, local string) (string, bool) {
	if n == nil {
		return "", false
	}
	for _, attr := range n.Attrs {
		if attr.Name.Space == namespace && attr.Name.Local == local {
			return attr.Value, true
		}
	}
	return "", false
}

// attrValue returns the attribute value or fallback when absent.
func (n *xmlNode) attrValue(namespace, local, fallback string) string {
	if value, ok := n.attrNS(namespace, local); ok {
		return value
	}
	return fallback
}

// walkDescendants visits node and all descendants in document order.
func (n *xmlNode) walkDescendants(visit func(*xmlNode) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for _, child := range n.Children {
		child.walkDescendants(visit)
	}
}

// ancestorWhere returns the nearest ancestor matching the predicate, mirroring
// lxml iterancestors.
func (n *xmlNode) ancestorWhere(match func(*xmlNode) bool) *xmlNode {
	for current := n.Parent; current != nil; current = current.Parent {
		if match(current) {
			return current
		}
	}
	return nil
}

// hasDescendant reports whether node or any descendant matches ns:local.
func (n *xmlNode) hasDescendant(namespace, local string) bool {
	found := false
	n.walkDescendants(func(candidate *xmlNode) bool {
		if isElement(candidate, namespace, local) {
			found = true
			return false
		}
		return true
	})
	return found
}

// descendantText concatenates the direct text of node and its descendants.
func (n *xmlNode) descendantText() string {
	var builder strings.Builder
	n.walkDescendants(func(candidate *xmlNode) bool {
		builder.WriteString(candidate.Text)
		return true
	})
	return builder.String()
}

// xmlName identifies one namespaced element step.
type xmlName struct {
	namespace string
	local     string
}

func wordName(local string) xmlName    { return xmlName{namespace: nsWord, local: local} }
func mathName(local string) xmlName    { return xmlName{namespace: nsMath, local: local} }
func drawingName(local string) xmlName { return xmlName{namespace: nsDrawingML, local: local} }
func compatName(local string) xmlName  { return xmlName{namespace: nsMarkupCompat, local: local} }
func vmlName(local string) xmlName     { return xmlName{namespace: nsVML, local: local} }
