"""Ordered OOXML extraction with explicit assets and fail-closed math handling."""

from __future__ import annotations

import posixpath
import re
from pathlib import Path
from zipfile import ZipFile
from lxml import etree

from .document_readers import _validate_docx_archive, MAX_EXTRACTED_CHARACTERS

WORD_PARSER_VERSION = "word-assets-v1"
NS = {"w": "http://schemas.openxmlformats.org/wordprocessingml/2006/main",
      "m": "http://schemas.openxmlformats.org/officeDocument/2006/math",
      "r": "http://schemas.openxmlformats.org/officeDocument/2006/relationships",
      "a": "http://schemas.openxmlformats.org/drawingml/2006/main",
      "v": "urn:schemas-microsoft-com:vml",
      "wp": "http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing",
      "mc": "http://schemas.openxmlformats.org/markup-compatibility/2006"}
CAPTION = re.compile(r"^\s*(?P<kind>图|表|figure|fig\.?|table)\s*(?P<number>\d+(?:[.\-]\d+)*|[一二三四五六七八九十]+)(?:\b|(?=[\s:：、.．]))", re.I)
NOTE = re.compile(r"^\s*(?:注|说明|来源|资料来源|note|notes|source)\s*[:：.]", re.I)


def tag(name):
    prefix, local = name.split(":")
    return f"{{{NS[prefix]}}}{local}"


def xml(data):
    return etree.fromstring(data, etree.XMLParser(resolve_entities=False, no_network=True, load_dtd=False))


def formula(node):
    from docxlatex.parser import OMMLParser
    from xml.etree import ElementTree

    parser = OMMLParser()
    alphabets = {"roman": None, "script": "mathcal", "fraktur": "mathfrak", "double-struck": "mathbb",
                 "sans-serif": "mathsf", "monospace": "mathtt"}
    def styled_run(converter, element):
        value = OMMLParser.parse_r(converter, element)
        script = element.find("m:rPr/m:scr", NS)
        alphabet = alphabets.get(script.get(tag("m:val"))) if script is not None else None
        if alphabet:
            value = "\\" + alphabet + "{" + value + "}"
        style = element.find("m:rPr/m:sty", NS)
        command = {"b": "mathbf", "bi": "boldsymbol", "p": "mathrm"}.get(style.get(tag("m:val"))) if style is not None else None
        if command and not (alphabet and command == "mathrm"):
            value = "\\" + command + "{" + value + "}"
        return value
    parser.parsers = {**parser.parsers, tag("m:r"): styled_run}
    for operator in node.findall(".//m:naryPr/m:chr", NS):
        value = operator.get(tag("m:val"), "")
        if len(value) != 1 or ord(value) not in {8719, 8720, 8721, 8747, 8748, 8749, 8750, 8751, 8752, 8896, 8897, 8898, 8899}:
            raise ValueError(f"Unsupported Word formula operator: {value}")
    for script in node.findall(".//m:rPr/m:scr", NS):
        if script.get(tag("m:val")) not in alphabets:
            raise ValueError("Unsupported styled Word formula alphabet")
    wrappers = {"oMath", "num", "den", "sub", "sup", "deg", "fName"}
    required = {"f": ("num", "den"), "sSup": ("e", "sup"), "sSub": ("e", "sub"),
                "sSubSup": ("e", "sub", "sup"), "rad": ("e",), "m": ("mr",)}
    def validate(element):
        local = etree.QName(element).localname
        if local.endswith("Pr"):
            return
        if etree.QName(element).namespace == NS["m"]:
            if element.tag not in parser.parsers and local not in wrappers:
                raise ValueError(f"Unsupported Word formula structure: {local}")
            for needed in required.get(local, ()):
                child = element.find(tag(f"m:{needed}"))
                # Empty arguments are explicit Word placeholders, not missing XML structures.
                if child is None:
                    raise ValueError(f"Incomplete Word formula: {local}/{needed}")
        for child in element:
            validate(child)
    validate(node)
    try:
        result = parser.parse(ElementTree.fromstring(etree.tostring(node))).strip()
    except (KeyError, TypeError, ValueError, AttributeError) as exc:
        raise ValueError(f"Word formula conversion failed: {exc}") from exc
    if not result:
        raise ValueError("Word formula conversion returned no text")
    return result


class WordReader:
    def __init__(self, archive, output):
        self.archive = archive
        self.output = output
        self.blocks = []
        self.assets = []
        self.diagnostics = []
        self.headings = []
        self.paragraph = 0
        self.section = 0
        self.parts = {}
        self.relations = {}
        self.visited_notes = set()
        self.characters = 0
        self.serial = 0
        self.styles = {}
        if "word/styles.xml" in archive.namelist():
            for style in self.part("word/styles.xml").findall(tag("w:style")):
                name = style.find(tag("w:name"))
                based = style.find(tag("w:basedOn"))
                self.styles[style.get(tag("w:styleId"))] = (
                    name.get(tag("w:val"), "") if name is not None else "",
                    based.get(tag("w:val")) if based is not None else None)
        types = xml(archive.read("[Content_Types].xml"))
        self.types = {el.get("Extension", "").lower(): el.get("ContentType") for el in types if el.get("Extension")}
        self.overrides = {el.get("PartName", "").lstrip("/"): el.get("ContentType") for el in types if el.get("PartName")}

    def part(self, name):
        if name not in self.parts:
            self.parts[name] = xml(self.archive.read(name))
        return self.parts[name]

    def rels(self, part):
        if part not in self.relations:
            folder, name = posixpath.split(part)
            relpath = f"{folder}/_rels/{name}.rels"
            self.relations[part] = {}
            if relpath in self.archive.namelist():
                for rel in self.part(relpath):
                    target = rel.get("Target", "")
                    resolved = posixpath.normpath(posixpath.join(folder, target)) if not target.startswith("/") else target.lstrip("/")
                    self.relations[part][rel.get("Id")] = (resolved, rel.get("TargetMode") == "External", rel.get("Type", ""))
        return self.relations[part]

    def location(self, part, scope):
        return {"part": part, "paragraph": self.paragraph, "headings": list(self.headings), "scope": scope,
                "section": self.section}

    def block(self, kind, text, location, **extra):
        self.characters += len(text)
        if self.characters > MAX_EXTRACTED_CHARACTERS:
            raise ValueError("Word extracted text exceeds the size limit")
        self.serial += 1
        item = {"ref": f"block-{self.serial}", "order": len(self.blocks), "kind": kind,
                "text": text, "location": dict(location), **extra}
        self.blocks.append(item)
        return item

    def walk(self, parent, part, scope):
        for node in parent:
            if node.tag == tag("w:p"):
                self.read_paragraph(node, part, scope)
            elif node.tag == tag("w:tbl"):
                self.read_table(node, part, scope)
            elif node.tag in {tag("w:del"), tag("w:moveFrom"), tag("w:sdtPr"), tag("w:sectPr")}:
                continue
            elif node.tag == tag("mc:AlternateContent"):
                choice = node.find(tag("mc:Choice"))
                self.walk(choice if choice is not None else node.find(tag("mc:Fallback")), part, scope)
            elif node.tag in {tag("w:altChunk"), tag("w:subDoc")}:
                raise ValueError(f"Unsupported embedded Word content in {part}: {etree.QName(node).localname}")
            else:
                self.walk(node, part, scope)

    def style(self, paragraph):
        style = paragraph.find("w:pPr/w:pStyle", NS)
        value = style.get(tag("w:val"), "") if style is not None else ""
        names, seen = [], set()
        while value and value not in seen:
            seen.add(value)
            name, based = self.styles.get(value, (value, None))
            names.extend([value, name])
            value = based
        return names

    def read_paragraph(self, node, part, scope, emit=True):
        self.paragraph += 1
        location = self.location(part, scope)
        styles = self.style(node)
        heading = next((re.fullmatch(r"heading\s*([1-9])", name, re.I) for name in styles
                        if re.fullmatch(r"heading\s*([1-9])", name, re.I)), None)
        offset = len(self.blocks)
        raw = self.inline(node, part, scope)
        text = re.sub(r"\x00[^\x00]+\x00", "", raw).strip()
        if heading and scope == "body":
            level = int(heading[1])
            self.headings = self.headings[:level-1] + [text]
            self.section += 1
            location = self.location(part, scope)
        if emit:
            events = {b["ref"]: b for b in self.blocks[offset:]}
            del self.blocks[offset:]
            caption = CAPTION.match(text)
            kind = "heading" if heading else "caption" if caption or any("caption" in s.lower() for s in styles) else "note" if NOTE.match(text) else "paragraph"
            for segment in re.split(r"(\x00[^\x00]+\x00)", raw):
                if segment.startswith("\x00"):
                    self.blocks.append(events[segment[1:-1]])
                elif segment.strip():
                    self.block(kind, segment.strip(), location,
                               bookmarks=[el.get(tag("w:name")) for el in node.iter(tag("w:bookmarkStart"))],
                               references=re.findall(r"\bREF\s+([^\s\\]+)", " ".join(node.xpath('.//w:instrText/text() | .//w:fldSimple/@w:instr', namespaces=NS))))
        elif text and (CAPTION.match(text) or NOTE.match(text)):
            self.block("caption" if CAPTION.match(text) else "note", text, location, in_cell=True)
        if not emit:
            self.characters += len(text)
            if self.characters > MAX_EXTRACTED_CHARACTERS:
                raise ValueError("Word extracted text exceeds the size limit")
        return text

    def inline(self, node, part, scope):
        if node.tag in {tag("w:del"), tag("w:moveFrom"), tag("w:instrText"), tag("w:pPr"), tag("w:rPr"), tag("w:sdtPr")}:
            return ""
        if node.tag == tag("w:t"):
            return node.text or ""
        if node.tag in {tag("w:tab"), tag("w:br"), tag("w:cr")}:
            return "\t" if node.tag == tag("w:tab") else "\n"
        if node.tag == tag("w:noBreakHyphen"):
            return "\u2011"
        if node.tag == tag("w:softHyphen"):
            return "\u00ad"
        if node.tag == tag("w:ptab"):
            return "\t"
        if node.tag == tag("w:sym"):
            raise ValueError(f"Unsupported legacy symbol font at {part}, paragraph {self.paragraph}")
        if node.tag == tag("a:graphicData") and not any(n.tag in {tag("a:blip"), tag("w:txbxContent")} for n in node.iter()):
            raise ValueError(f"Unsupported embedded chart or diagram at {part}, paragraph {self.paragraph}")
        if node.tag == tag("m:oMath"):
            return "$" + formula(node) + "$"
        if node.tag == tag("w:object"):
            raise ValueError(f"Unsupported embedded OLE object at {part}, paragraph {self.paragraph}")
        if node.tag in {tag("a:blip"), tag("v:imagedata")}:
            return "\x00" + self.picture(node, part, scope) + "\x00"
        if node.tag == tag("w:txbxContent"):
            offset = len(self.blocks)
            self.walk(node, part, scope + f"/textbox-{self.paragraph}")
            return "".join("\x00" + b["ref"] + "\x00" for b in self.blocks[offset:])
        if node.tag in {tag("w:footnoteReference"), tag("w:endnoteReference")}:
            note_kind = "footnote" if node.tag == tag("w:footnoteReference") else "endnote"
            note_id = node.get(tag("w:id"))
            key = (note_kind, note_id)
            if key not in self.visited_notes:
                offset = len(self.blocks)
                self.visited_notes.add(key)
                note_part = f"word/{note_kind}s.xml"
                matching = [n for n in self.part(note_part) if n.get(tag("w:id")) == note_id]
                if not matching:
                    raise ValueError(f"Missing {note_kind} {note_id}")
                self.walk(matching[0], note_part, f"{note_kind}-{note_id}")
                return f" [{note_kind} {note_id}]" + "".join("\x00" + b["ref"] + "\x00" for b in self.blocks[offset:])
            return f" [{note_kind} {note_id}]"
        if node.tag == tag("mc:AlternateContent"):
            choice = node.find(tag("mc:Choice"))
            fallback = node.find(tag("mc:Fallback"))
            selected = choice if choice is not None else fallback
            return self.inline(selected, part, scope) if selected is not None else ""
        return "".join(self.inline(child, part, scope) for child in node)

    def picture(self, node, part, scope):
        rid = node.get(tag("r:embed")) or node.get(tag("r:id")) or node.get(tag("r:link"))
        target, external, _ = self.rels(part).get(rid, (None, False, ""))
        if external or not target or target.startswith("../") or target not in self.archive.namelist():
            raise ValueError(f"Missing or external Word image at {part}, paragraph {self.paragraph}")
        mime = self.overrides.get(target) or self.types.get(posixpath.splitext(target)[1][1:].lower())
        if not mime or not mime.startswith("image/"):
            raise ValueError(f"Unsupported image content type: {target}")
        data = self.archive.read(target)
        if not data:
            raise ValueError(f"Empty Word image: {target}")
        asset_id = f"picture-{len(self.assets)+1}"
        filename = f"images/{asset_id}{posixpath.splitext(target)[1]}"
        if self.output is not None:
            destination = self.output / filename
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(data)
        location = self.location(part, scope)
        asset = {"asset_id": asset_id, "kind": "picture", "image_file": filename, "mime_type": mime,
                 "caption": "", "caption_source": "none", "generated_description": "", "captions": [],
                 "footnotes": [], "table": None, "location": location, "labels": [], "bookmarks": []}
        container = next((p for p in node.iterancestors() if p.tag in {tag("w:drawing"), tag("w:pict")}), None)
        if container is not None:
            for props in container.iter(tag("wp:docPr")):
                asset["labels"].extend(props.get(key, "") for key in ("name", "title", "descr"))
        paragraph = next((p for p in node.iterancestors() if p.tag == tag("w:p")), None)
        if paragraph is not None:
            asset["bookmarks"] = [b.get(tag("w:name")) for b in paragraph.iter(tag("w:bookmarkStart"))]
        self.assets.append(asset)
        return self.block("picture", "", location, asset_id=asset_id)["ref"]

    def read_table(self, node, part, scope, parent_cell=None):
        asset_id = f"table-{len(self.assets)+1}"
        location = self.location(part, scope)
        table = {"rows": [], "cells": [], "headers": [], "header_rows": [], "num_rows": 0,
                 "num_cols": 0, "footnotes": [], "parent_cell": parent_cell}
        asset = {"asset_id": asset_id, "kind": "table", "image_file": None, "mime_type": None,
                 "caption": "", "caption_source": "none", "generated_description": "", "captions": [],
                 "footnotes": [], "table": table, "location": location, "labels": [],
                 "bookmarks": [b.get(tag("w:name")) for b in node.iter(tag("w:bookmarkStart"))]}
        self.assets.append(asset)
        self.block("table", "", location, asset_id=asset_id)
        grid = node.find(tag("w:tblGrid"))
        columns = len(grid) if grid is not None else 0
        active = {}
        # Only direct rows belong to this table; nested rows are processed in their own asset.
        def children(element, wanted):
            for child in element:
                if child.tag == tag(wanted):
                    yield child
                elif child.tag in {tag("w:sdt"), tag("w:sdtContent"), tag("w:ins"), tag("w:moveTo"), tag("w:customXml")}:
                    yield from children(child, wanted)
        for row_number, row in enumerate(children(node, "w:tr")):
            before = row.find("w:trPr/w:gridBefore", NS)
            column = int(before.get(tag("w:val"), "0")) if before is not None else 0
            values = [""] * column
            next_active = {}
            for cell in children(row, "w:tc"):
                span_node = cell.find("w:tcPr/w:gridSpan", NS)
                span = int(span_node.get(tag("w:val"), "1")) if span_node is not None else 1
                if span < 1 or column + span > 1000:
                    raise ValueError(f"Invalid Word table grid: {asset_id}")
                vmerge = cell.find("w:tcPr/w:vMerge", NS)
                continuation = vmerge is not None and vmerge.get(tag("w:val"), "continue") == "continue"
                if cell.find("w:tcPr/w:hMerge", NS) is not None:
                    raise ValueError(f"Unsupported legacy horizontal table merge: {asset_id}")
                text, nested = [], []
                cell_scope = f"{scope}/{asset_id}/r{row_number}c{column}"
                def content(container):
                    for child in container:
                        if child.tag == tag("w:p"):
                            value = self.read_paragraph(child, part, cell_scope, emit=False)
                            if value:
                                text.append(value)
                        elif child.tag == tag("w:tbl"):
                            nested_id = self.read_table(child, part, cell_scope,
                                                        {"asset_id": asset_id, "row": row_number, "col": column})
                            nested.append(nested_id)
                        elif child.tag in {tag("w:sdt"), tag("w:sdtContent"), tag("w:ins"), tag("w:moveTo"), tag("w:customXml")}:
                            content(child)
                        elif child.tag in {tag("w:altChunk"), tag("w:object")}:
                            raise ValueError(f"Unsupported embedded content in {asset_id}")
                content(cell)
                if continuation:
                    anchor = active.get(column)
                    if anchor is None or anchor["col_span"] != span:
                        raise ValueError(f"Broken vertical table merge: {asset_id}, row {row_number+1}")
                    if text or nested:
                        raise ValueError(f"Unexpected content in merged continuation: {asset_id}")
                    anchor["row_span"] += 1
                    next_active[column] = anchor
                    values.extend([""] * span)
                else:
                    value = "\n".join(text)
                    item = {"row": row_number, "col": column, "row_span": 1, "col_span": span,
                            "text": value, "nested_tables": nested}
                    table["cells"].append(item)
                    values.extend([value] + [""] * (span-1))
                    if vmerge is not None:
                        next_active[column] = item
                column += span
            active = next_active
            after = row.find("w:trPr/w:gridAfter", NS)
            if after is not None:
                values.extend([""] * int(after.get(tag("w:val"), "0")))
            columns = max(columns, len(values))
            table["rows"].append(values)
            if row.find("w:trPr/w:tblHeader", NS) is not None:
                table["header_rows"].append(values)
        if not table["rows"] or not columns:
            raise ValueError(f"Empty Word table: {asset_id}")
        for row in table["rows"]:
            row.extend([""] * (columns-len(row)))
        table["num_rows"], table["num_cols"] = len(table["rows"]), columns
        table["headers"] = list(table["rows"][0])
        if not table["header_rows"]:
            table["header_rows"] = [list(table["headers"])]
        return asset_id

    def associate(self):
        assets = {asset["asset_id"]: asset for asset in self.assets}
        skipped = set()
        for index, block in enumerate(self.blocks):
            if block["kind"] not in {"caption", "note"}:
                continue
            location = block["location"]
            same = [a for a in self.assets if all(a["location"][key] == location[key]
                    for key in ("part", "scope", "section"))]
            match = CAPTION.match(block["text"])
            kind = ("table" if match["kind"].lower() in {"table", "表"} else "picture") if match else None
            if kind:
                same = [a for a in same if a["kind"] == kind]
            explicit = [a for a in same if set(block.get("references", [])).intersection(a.get("bookmarks", []))]
            if block["kind"] == "note" and index:
                previous = self.blocks[index-1]
                if previous["kind"] in {"caption", "note"}:
                    explicit = [a for a in same if a["asset_id"] in previous.get("associated_assets", [])]
            if match:
                for asset in same:
                    if any((label_match := CAPTION.match(label)) and label_match["number"] == match["number"]
                           and (label_match["kind"].lower() in {"table", "表"}) == (kind == "table")
                           for label in asset["labels"]):
                        if asset not in explicit:
                            explicit.append(asset)
                numbered = [b for b in self.blocks if b["kind"] == "caption"
                            and all(b["location"][k] == location[k] for k in ("part", "scope", "section"))
                            and (m := CAPTION.match(b["text"])) and (m["kind"].lower() in {"table", "表"}) == (kind == "table")]
                numbers = [CAPTION.match(b["text"])["number"] for b in numbered]
                if not explicit and len(numbers) == len(same) and set(numbers) == {str(i+1) for i in range(len(same))}:
                    explicit = [same[int(match["number"])-1]]
            adjacent = []
            for direction in (-1, 1):
                cursor = index + direction
                while 0 <= cursor < len(self.blocks):
                    neighbor = self.blocks[cursor]
                    if neighbor["location"]["section"] != location["section"] or neighbor["location"]["part"] != location["part"]:
                        break
                    if neighbor["location"]["scope"] != location["scope"]:
                        if neighbor["location"]["scope"].startswith(location["scope"] + "/"):
                            cursor += direction
                            continue
                        break
                    if neighbor.get("asset_id"):
                        asset = assets[neighbor["asset_id"]]
                        if asset in same:
                            adjacent.append(asset)
                            cursor += direction
                            continue
                        break
                    if neighbor["kind"] not in {"caption", "note"}:
                        break
                    cursor += direction
            candidates = explicit or adjacent
            # Notes belong below an object, not to the next object above which they happen to sit.
            if block["kind"] == "note":
                candidates = [a for a in candidates if next(b["order"] for b in self.blocks if b.get("asset_id") == a["asset_id"]) < block["order"]]
            unique = {a["asset_id"]: a for a in candidates}
            if len(unique) == 1:
                asset = next(iter(unique.values()))
                annotation = {"text": block["text"], "ref": block["ref"], "location": location}
                key = "footnotes" if block["kind"] == "note" else "captions"
                asset[key].append(annotation)
                block["associated_assets"] = [asset["asset_id"]]
                if key == "captions":
                    asset["caption"] = "\n".join(item["text"] for item in asset[key])
                    asset["caption_source"] = "document"
                elif asset["table"] is not None:
                    asset["table"]["footnotes"].append(block["text"])
            elif block["kind"] == "caption":
                affected = list(unique.values()) or same
                for asset in affected:
                    skipped.add(asset["asset_id"])
                self.diagnostics.append({"kind": "unassociated_caption", "message": "Caption could not be uniquely associated; affected objects were skipped.",
                                         "location": location, "text": block["text"], "asset_ids": [a["asset_id"] for a in affected]})
        # Descendants of a skipped table must not remain independently indexed.
        changed = True
        while changed:
            previous = len(skipped)
            for asset in self.assets:
                if any(f"/{item}/" in asset["location"]["scope"] for item in skipped):
                    skipped.add(asset["asset_id"])
            changed = len(skipped) != previous
        for asset in self.assets:
            if asset["asset_id"] in skipped and self.output is not None and asset["image_file"]:
                (self.output / asset["image_file"]).unlink(missing_ok=True)
        self.assets = [a for a in self.assets if a["asset_id"] not in skipped]
        self.blocks = [b for b in self.blocks if b.get("asset_id") not in skipped]
        for block in self.blocks:
            block["associated_assets"] = [a for a in block.get("associated_assets", []) if a not in skipped]
        return len(skipped)

    def read(self):
        body = self.part("word/document.xml").find(tag("w:body"))
        if body is None:
            raise ValueError("Word document has no body")
        self.walk(body, "word/document.xml", "body")
        seen = set()
        for reference in body.iter():
            if reference.tag not in {tag("w:headerReference"), tag("w:footerReference")}:
                continue
            target, external, _ = self.rels("word/document.xml").get(reference.get(tag("r:id")), (None, False, ""))
            if external or not target:
                raise ValueError("Invalid Word header/footer reference")
            if target not in seen:
                seen.add(target)
                self.headings = []
                self.walk(self.part(target), target, posixpath.basename(target))
        for order, block in enumerate(self.blocks):
            block["order"] = order
        skipped_count = self.associate()
        for order, block in enumerate(self.blocks):
            block["order"] = order
        return {"version": WORD_PARSER_VERSION, "blocks": self.blocks, "assets": self.assets,
                "diagnostics": self.diagnostics, "skipped_object_count": skipped_count}


def parse_docx(path: Path, output: Path | None = None):
    _validate_docx_archive(path)
    try:
        with ZipFile(path) as archive:
            return WordReader(archive, output).read()
    except ValueError:
        raise
    except (KeyError, etree.XMLSyntaxError, OSError, TypeError) as exc:
        raise ValueError(f"Word document is invalid or incomplete: {path.name}: {exc}") from exc


def render_word(manifest):
    assets = {a["asset_id"]: a for a in manifest["assets"]}
    parts = []
    for block in manifest["blocks"]:
        asset = assets.get(block.get("asset_id"))
        if asset:
            if asset["kind"] == "table":
                parts.append("\n".join(" | ".join(str(cell).replace("|", r"\|").replace("\n", " ") for cell in row)
                                       for row in asset["table"]["rows"]))
            else:
                parts.append(f"[Image: {asset['asset_id']}]")
        elif block["text"] and not block.get("in_cell"):
            parts.append(block["text"])
    return "\n\n".join(parts)
