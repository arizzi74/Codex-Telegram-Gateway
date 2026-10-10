package gateway

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Large answers remain deliverable as plain text without allocating a Markdown
// syntax tree. This is a rendering bound, not a content truncation limit.
const telegramMarkdownMaxSource = 1 << 20

// Leave room for a session heading added by the delivery layer. Telegram accepts
// at most 100 entities per message, independently of its text length limit.
const telegramFormattedEntityLimit = 99

// renderTelegramMarkdown converts CommonMark plus GFM tables, strikethrough and
// task lists to Telegram's literal text and explicit UTF-16 entities. It never
// asks Telegram to parse HTML or Markdown supplied by a model.
func renderTelegramMarkdown(source string) (string, []TelegramEntity) {
	source = strings.ReplaceAll(strings.ToValidUTF8(source, "\uFFFD"), "\x00", "\uFFFD")
	if len(source) > telegramMarkdownMaxSource {
		return source, nil
	}
	parser := goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList))
	r := telegramMarkdownRenderer{source: []byte(source)}
	r.render(parser.Parser().Parse(text.NewReader(r.source)), 0)
	if r.out.Len() == 0 {
		return source, nil
	}
	sortTelegramEntities(r.entities)
	return r.out.String(), r.entities
}

type telegramMarkdownRenderer struct {
	source       []byte
	out          strings.Builder
	units        int
	newlines     int
	styles       []TelegramEntity
	entities     []TelegramEntity
	lastRun      []int
	listDepth    int
	linkUnits    int
	literalLinks bool
}

func telegramUTF16Len(s string) int {
	length := 0
	for _, r := range s {
		length++
		if r > 0xffff {
			length++
		}
	}
	return length
}

func sameTelegramStyle(a, b TelegramEntity) bool {
	return a.Type == b.Type && a.URL == b.URL && a.Language == b.Language
}

func (r *telegramMarkdownRenderer) write(value string) {
	r.writeStyles(value, r.styles)
}

func (r *telegramMarkdownRenderer) writeStyles(value string, styles []TelegramEntity) {
	if value == "" {
		return
	}
	length := telegramUTF16Len(value)
	for _, style := range styles {
		if style.Type == "text_link" {
			r.linkUnits += length
		}
	}
	merge := len(styles) == len(r.lastRun)
	if merge {
		for i, style := range styles {
			previous := r.entities[r.lastRun[i]]
			if !sameTelegramStyle(previous, style) || previous.Offset+previous.Length != r.units {
				merge = false
				break
			}
		}
	}
	if merge {
		for _, index := range r.lastRun {
			r.entities[index].Length += length
		}
	} else {
		r.lastRun = r.lastRun[:0]
		for _, style := range styles {
			style.Offset, style.Length = r.units, length
			r.lastRun = append(r.lastRun, len(r.entities))
			r.entities = append(r.entities, style)
		}
	}
	r.out.WriteString(value)
	r.units += length
	for _, ch := range value {
		if ch == '\n' {
			r.newlines++
		} else {
			r.newlines = 0
		}
	}
}

func (r *telegramMarkdownRenderer) gap(newlines int) {
	if r.units > 0 && r.newlines < newlines {
		r.writeStyles(strings.Repeat("\n", newlines-r.newlines), nil)
	}
}

func (r *telegramMarkdownRenderer) withStyle(style TelegramEntity, render func()) {
	for _, active := range r.styles {
		if sameTelegramStyle(active, style) || active.Type == "text_link" && style.Type == "text_link" {
			render()
			return
		}
	}
	r.styles = append(r.styles, style)
	render()
	r.styles = r.styles[:len(r.styles)-1]
}

func (r *telegramMarkdownRenderer) children(node ast.Node, depth int) {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		r.render(child, depth+1)
	}
}

func (r *telegramMarkdownRenderer) blocks(node ast.Node, depth int) {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if child != node.FirstChild() {
			r.gap(2)
		}
		r.render(child, depth+1)
	}
}

func (r *telegramMarkdownRenderer) render(node ast.Node, depth int) {
	// Avoid recursive rendering of pathological nested input. The iterative
	// fallback preserves visible leaf content and simply omits its formatting.
	if depth > 128 {
		r.renderPlain(node)
		return
	}
	switch n := node.(type) {
	case *ast.Document:
		r.blocks(n, depth)
	case *ast.Text:
		value := n.Value(r.source)
		if !n.IsRaw() {
			value = telegramMarkdownUnescape(value)
		}
		r.write(string(value))
		if n.HardLineBreak() || n.SoftLineBreak() {
			r.write("\n")
		}
	case *ast.String:
		value := n.Value
		if !n.IsRaw() {
			value = telegramMarkdownUnescape(value)
		}
		r.write(string(value))
	case *ast.Paragraph, *ast.TextBlock:
		r.children(node, depth)
	case *ast.Heading:
		r.withStyle(TelegramEntity{Type: "bold"}, func() { r.children(n, depth) })
	case *ast.Emphasis:
		kind := "italic"
		if n.Level == 2 {
			kind = "bold"
		}
		r.withStyle(TelegramEntity{Type: kind}, func() { r.children(n, depth) })
	case *extast.Strikethrough:
		r.withStyle(TelegramEntity{Type: "strikethrough"}, func() { r.children(n, depth) })
	case *ast.CodeSpan:
		// Code/pre cannot overlap any other Telegram entity, including links.
		var code strings.Builder
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if part, ok := child.(*ast.Text); ok {
				code.Write(part.Value(r.source))
				if part.SoftLineBreak() {
					code.WriteByte(' ')
				}
			}
		}
		r.writeStyles(strings.ReplaceAll(code.String(), "\n", " "), []TelegramEntity{{Type: "code"}})
	case *ast.FencedCodeBlock:
		language := string(n.Language(r.source))
		if len(language) > 64 || strings.IndexFunc(language, func(ch rune) bool { return unicode.IsSpace(ch) || unicode.IsControl(ch) }) >= 0 {
			language = ""
		}
		r.writeStyles(string(n.Lines().Value(r.source)), []TelegramEntity{{Type: "pre", Language: language}})
	case *ast.CodeBlock:
		r.writeStyles(string(n.Lines().Value(r.source)), []TelegramEntity{{Type: "pre"}})
	case *ast.Link:
		r.link(n, string(telegramMarkdownUnescape(n.Destination)), depth)
	case *ast.Image:
		r.write("Image: ")
		r.link(n, string(telegramMarkdownUnescape(n.Destination)), depth)
	case *ast.AutoLink:
		label, destination := string(n.Label(r.source)), string(n.URL(r.source))
		if telegramMarkdownHTTPURL(destination) {
			r.withStyle(TelegramEntity{Type: "text_link", URL: destination}, func() { r.write(label) })
		} else {
			r.write(label)
			if label != destination {
				r.write(" (" + destination + ")")
			}
		}
	case *ast.RawHTML:
		r.write(string(n.Segments.Value(r.source)))
	case *ast.HTMLBlock:
		r.write(string(n.Lines().Value(r.source)))
		if n.HasClosure() {
			r.write(string(n.ClosureLine.Value(r.source)))
		}
	case *ast.List:
		r.list(n, depth)
	case *ast.Blockquote:
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if child != n.FirstChild() {
				r.gap(2)
			}
			r.write("> ")
			r.render(child, depth+1)
		}
	case *ast.ThematicBreak:
		r.write("────────")
	case *extast.TaskCheckBox:
		if n.IsChecked {
			r.write("☑ ")
		} else {
			r.write("☐ ")
		}
	case *extast.Table:
		r.table(n, depth)
	default:
		r.children(node, depth)
	}
}

func telegramMarkdownUnescape(value []byte) []byte {
	// Resolve escapes and references in one pass. Resolving references after
	// removing all backslashes would turn the literal \&amp; into an ampersand.
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] == '\\' && i+1 < len(value) && util.IsPunct(value[i+1]) {
			out.WriteByte(value[i+1])
			i += 2
			continue
		}
		if value[i] == '&' {
			end := i + 1
			for end < len(value) && end-i <= 32 && value[end] != ';' && (value[end] == '#' || util.IsAlphaNumeric(value[end])) {
				end++
			}
			if end < len(value) && value[end] == ';' {
				reference := value[i : end+1]
				resolved := util.ResolveEntityNames(util.ResolveNumericReferences(reference))
				// Telegram removes these characters after accepting entity text.
				// Introducing them here could join text across a redacted secret
				// when Telegram subsequently cleans the outgoing message.
				if strings.IndexFunc(string(resolved), func(ch rune) bool {
					return ch == '\r' || ch >= '\u2028' && ch <= '\u202e' || ch == '\u0333' || ch == '\u033f' || ch == '\u030a'
				}) >= 0 {
					out.Write(reference)
				} else {
					out.Write(resolved)
				}
				i = end + 1
				continue
			}
		}
		out.WriteByte(value[i])
		i++
	}
	return []byte(out.String())
}

func telegramMarkdownHTTPURL(destination string) bool {
	if strings.IndexFunc(destination, func(ch rune) bool { return unicode.IsSpace(ch) || unicode.IsControl(ch) }) >= 0 {
		return false
	}
	u, err := url.Parse(destination)
	if err != nil || (!strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http")) || u.Opaque != "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	host := u.Hostname()
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	if strings.HasPrefix(u.Host, "[") && net.ParseIP(host) == nil {
		return false
	}
	// Telegram rejects single-label web hosts, such as localhost.
	return strings.Trim(host, ".") != "" && (strings.Contains(host, ".") || net.ParseIP(host) != nil)
}

func (r *telegramMarkdownRenderer) link(node ast.Node, destination string, depth int) {
	start := r.units
	startByte := r.out.Len()
	label := func() {
		r.children(node, depth)
		if r.units == start {
			r.write(destination)
		}
	}
	if r.literalLinks {
		label()
		if destination != "" && r.out.String()[startByte:] != destination {
			r.write(" (" + destination + ")")
		}
		return
	}
	for _, style := range r.styles {
		if style.Type == "text_link" {
			// A linked image can contain a different image URL. Telegram cannot
			// nest links; retain its destination visibly under the outer link.
			label()
			if destination != "" && destination != style.URL {
				r.write(" (" + destination + ")")
			}
			return
		}
	}
	if telegramMarkdownHTTPURL(destination) {
		before := r.linkUnits
		r.withStyle(TelegramEntity{Type: "text_link", URL: destination}, func() {
			label()
			if r.linkUnits == before {
				// A code-only label cannot also be a link. Keep the code and add
				// an independently clickable, visible destination beside it.
				r.write(" (" + destination + ")")
			}
		})
		return
	}
	label()
	if destination != "" && r.out.String()[startByte:] != destination {
		r.write(" (" + destination + ")")
	}
}

func (r *telegramMarkdownRenderer) list(node *ast.List, depth int) {
	r.listDepth++
	defer func() { r.listDepth-- }()
	index := node.Start
	for item := node.FirstChild(); item != nil; item = item.NextSibling() {
		if item != node.FirstChild() {
			r.gap(1)
		}
		prefix := strings.Repeat("  ", r.listDepth-1)
		if node.IsOrdered() {
			prefix += fmt.Sprintf("%d. ", index)
			index++
		} else {
			prefix += "• "
		}
		r.write(prefix)
		for child := item.FirstChild(); child != nil; child = child.NextSibling() {
			if child != item.FirstChild() {
				if _, nestedList := child.(*ast.List); nestedList {
					r.gap(1)
				} else {
					r.gap(2)
				}
			}
			r.render(child, depth+1)
		}
	}
}

// Tables become row cards: each value is paired with its column name. Unlike a
// padded monospace grid, this remains readable with long model IDs on phones.
func (r *telegramMarkdownRenderer) table(node *extast.Table, depth int) {
	header := node.FirstChild()
	if header == nil {
		return
	}
	var labels []string
	for cell := header.FirstChild(); cell != nil; cell = cell.NextSibling() {
		label := telegramMarkdownRenderer{source: r.source, literalLinks: true}
		label.children(cell, depth)
		labels = append(labels, label.out.String())
	}
	if header.NextSibling() == nil {
		for i, label := range labels {
			if i > 0 {
				r.write("\n")
			}
			r.withStyle(TelegramEntity{Type: "bold"}, func() { r.write(label) })
		}
		return
	}
	for row := header.NextSibling(); row != nil; row = row.NextSibling() {
		if row != header.NextSibling() {
			r.gap(2)
		}
		column := 0
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			if column > 0 {
				r.gap(1)
			}
			label := fmt.Sprintf("Column %d", column+1)
			if column < len(labels) && labels[column] != "" {
				label = labels[column]
			}
			r.withStyle(TelegramEntity{Type: "bold"}, func() { r.write(label + ":") })
			r.write(" ")
			r.children(cell, depth)
			column++
		}
		// GFM ignores surplus cells. Preserve that source content rather than
		// silently throwing away fields from an imperfect model-produced table.
		last := row.LastChild()
		if last != nil && last.Lines().Len() > 0 {
			stop := last.Lines().At(last.Lines().Len() - 1).Stop
			lineEnd := stop
			for lineEnd < len(r.source) && r.source[lineEnd] != '\n' {
				lineEnd++
			}
			rest := strings.TrimSpace(string(r.source[stop:lineEnd]))
			rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(rest, "|"), "|"))
			if rest != "" {
				r.write("\nAdditional: " + rest)
			}
		}
	}
}

func (r *telegramMarkdownRenderer) renderPlain(root ast.Node) {
	stack := []ast.Node{root}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node.FirstChild() == nil {
			r.render(node, 0)
			continue
		}
		for child := node.LastChild(); child != nil; child = child.PreviousSibling() {
			stack = append(stack, child)
		}
	}
}

func sortTelegramEntities(entities []TelegramEntity) {
	sort.SliceStable(entities, func(i, j int) bool {
		if entities[i].Offset != entities[j].Offset {
			return entities[i].Offset < entities[j].Offset
		}
		return entities[i].Length > entities[j].Length
	})
}

// splitTelegramFormatted preserves literal text exactly, splits only between
// runes, and clips/rebases each entity into its outgoing chunk. Newline breaks
// are preferred when they do not leave most of the text budget unused.
func splitTelegramFormatted(value string, entities []TelegramEntity, limit int) []SendMessage {
	if limit < 2 {
		limit = 4000
	}
	if value == "" {
		return nil
	}
	type boundary struct{ bytes, units int }
	boundaries := []boundary{{}}
	units := 0
	for offset, ch := range value {
		if offset > 0 {
			boundaries = append(boundaries, boundary{offset, units})
		}
		units++
		if ch > 0xffff {
			units++
		}
	}
	boundaries = append(boundaries, boundary{len(value), units})
	valid := make([]TelegramEntity, 0, len(entities))
	for _, entity := range entities {
		if entity.Offset < 0 || entity.Length <= 0 || entity.Offset > units || entity.Length > units-entity.Offset {
			continue
		}
		start := sort.Search(len(boundaries), func(i int) bool { return boundaries[i].units >= entity.Offset })
		end := sort.Search(len(boundaries), func(i int) bool { return boundaries[i].units >= entity.Offset+entity.Length })
		if boundaries[start].units != entity.Offset || boundaries[end].units != entity.Offset+entity.Length {
			continue
		}
		valid = append(valid, entity)
	}
	sortTelegramEntities(valid)
	var chunks []SendMessage
	firstEntity := 0
	for start := 0; start < len(boundaries)-1; {
		startUnits := boundaries[start].units
		end := sort.Search(len(boundaries), func(i int) bool { return boundaries[i].units > startUnits+limit }) - 1
		for firstEntity < len(valid) && valid[firstEntity].Offset+valid[firstEntity].Length <= startUnits {
			firstEntity++
		}
		count := 0
		for _, entity := range valid[firstEntity:] {
			if entity.Offset >= boundaries[end].units {
				break
			}
			if entity.Offset+entity.Length <= startUnits {
				continue
			}
			count++
			if count > telegramFormattedEntityLimit && entity.Offset > startUnits {
				end = sort.Search(len(boundaries), func(i int) bool { return boundaries[i].units >= entity.Offset })
				break
			}
		}
		if end < len(boundaries)-1 {
			minimum := startUnits + (boundaries[end].units-startUnits)/2
			line, paragraph := 0, 0
			for candidate := start + 1; candidate <= end; candidate++ {
				position := boundaries[candidate].bytes
				if boundaries[candidate].units < minimum || value[position-1] != '\n' {
					continue
				}
				line = candidate
				if position >= 2 && value[position-2] == '\n' {
					paragraph = candidate
				}
			}
			if paragraph > 0 {
				end = paragraph
			} else if line > 0 {
				end = line
			}
		}
		chunk := SendMessage{Text: value[boundaries[start].bytes:boundaries[end].bytes]}
		endUnits := boundaries[end].units
		for _, entity := range valid[firstEntity:] {
			if entity.Offset >= endUnits {
				break
			}
			entityEnd := entity.Offset + entity.Length
			if entityEnd <= startUnits {
				continue
			}
			entity.Offset = max(entity.Offset, startUnits)
			entity.Length = min(entityEnd, endUnits) - entity.Offset
			entity.Offset -= startUnits
			chunk.Entities = append(chunk.Entities, entity)
		}
		chunks = append(chunks, chunk)
		start = end
	}
	return chunks
}
