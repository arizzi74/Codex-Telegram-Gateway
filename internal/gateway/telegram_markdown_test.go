package gateway

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func telegramEntityText(t *testing.T, value string, entity TelegramEntity) string {
	t.Helper()
	units := utf16.Encode([]rune(value))
	if entity.Offset < 0 || entity.Length < 1 || entity.Offset+entity.Length > len(units) {
		t.Fatalf("entity out of bounds: %+v in %q", entity, value)
	}
	end := entity.Offset + entity.Length
	if (units[entity.Offset] >= 0xdc00 && units[entity.Offset] <= 0xdfff) || (units[end-1] >= 0xd800 && units[end-1] <= 0xdbff) {
		t.Fatalf("entity cuts a surrogate pair: %+v in %q", entity, value)
	}
	return string(utf16.Decode(units[entity.Offset:end]))
}

func checkTelegramMarkdownEntities(t *testing.T, value string, entities []TelegramEntity) {
	t.Helper()
	for i, entity := range entities {
		telegramEntityText(t, value, entity)
		if entity.Type != "pre" && entity.Language != "" {
			t.Fatalf("language on non-pre entity: %+v", entity)
		}
		if entity.Type == "text_link" && !telegramMarkdownHTTPURL(entity.URL) {
			t.Fatalf("unsafe link entity: %+v", entity)
		}
		for _, other := range entities[i+1:] {
			start, end := entity.Offset, entity.Offset+entity.Length
			otherStart, otherEnd := other.Offset, other.Offset+other.Length
			if start >= otherEnd || otherStart >= end {
				continue
			}
			if start < otherStart && otherStart < end && end < otherEnd || otherStart < start && start < otherEnd && otherEnd < end {
				t.Fatalf("crossing spans: %+v %+v in %q", entity, other, value)
			}
			if entity.Type == "code" || entity.Type == "pre" || other.Type == "code" || other.Type == "pre" {
				t.Fatalf("code overlaps another entity: %+v %+v in %q", entity, other, value)
			}
			if entity.Type == "text_link" && other.Type == "text_link" {
				t.Fatalf("links overlap: %+v %+v in %q", entity, other, value)
			}
		}
	}
}

func TestTelegramMarkdownInlineStylesAndUTF16(t *testing.T) {
	value, entities := renderTelegramMarkdown("## 😀 **Status**\n\nPlain *italic*, **bold**, ~~old~~ and `x_y`.")
	if want := "😀 Status\n\nPlain italic, bold, old and x_y."; value != want {
		t.Fatalf("rendered text = %q, want %q", value, want)
	}
	want := map[string][]string{"bold": {"😀 Status", "bold"}, "italic": {"italic"}, "strikethrough": {"old"}, "code": {"x_y"}}
	got := map[string][]string{}
	for _, entity := range entities {
		got[entity.Type] = append(got[entity.Type], telegramEntityText(t, value, entity))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entities = %#v, want %#v", got, want)
	}
	checkTelegramMarkdownEntities(t, value, entities)
}

func TestTelegramMarkdownExclusiveCodeAndLinkedImages(t *testing.T) {
	for _, source := range []string{
		"**before `😀_code` after**",
		"[inline `code`](https://example.com)",
		"[`code`](https://example.com)",
		"**[bold link](https://example.com)**",
		"[![preview](https://img.example/1.png)](https://page.example/1)",
		"[<https://inner.example>](https://outer.example)",
		"![<https://inner.example>](https://img.example/)",
		"*a **b*****c**",
	} {
		t.Run(source, func(t *testing.T) {
			value, entities := renderTelegramMarkdown(source)
			checkTelegramMarkdownEntities(t, value, entities)
			if value == "" {
				t.Fatal("rendering discarded all content")
			}
			if strings.Contains(source, "img.example/1.png") && !strings.Contains(value, "https://img.example/1.png") {
				t.Fatalf("image destination lost: %q", value)
			}
			if source == "[`code`](https://example.com)" && value != "code (https://example.com)" {
				t.Fatalf("code-only link destination lost: %q", value)
			}
		})
	}
}

func TestTelegramMarkdownCodeBlocksKeepContentsAndLanguage(t *testing.T) {
	source := "```c++\n**raw** &amp; 😀\n```\n\n    indented <tag>\n\n` a\nb `"
	value, entities := renderTelegramMarkdown(source)
	checkTelegramMarkdownEntities(t, value, entities)
	var code []string
	for _, entity := range entities {
		code = append(code, telegramEntityText(t, value, entity))
		if entity.Type == "pre" && strings.Contains(telegramEntityText(t, value, entity), "**raw**") && entity.Language != "c++" {
			t.Fatalf("language lost: %+v", entity)
		}
	}
	if want := []string{"**raw** &amp; 😀\n", "indented <tag>\n", "a b"}; !reflect.DeepEqual(code, want) {
		t.Fatalf("code = %#v, want %#v", code, want)
	}
}

func TestTelegramMarkdownEscapesAndRawHTMLStayLiteral(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{`\*literal\* \&amp; &amp; &#x1f600;`, "*literal* &amp; & 😀"},
		{"<b>safe</b> and <script>alert('bad')</script>", "<b>safe</b> and <script>alert('bad')</script>"},
		{"<script>\nalert('bad')\n</script>", "<script>\nalert('bad')\n</script>"},
		{"unclosed **bold and `code", "unclosed **bold and `code"},
		{"```go\nunterminated **code**", "unterminated **code**\n"},
	} {
		t.Run(test.source, func(t *testing.T) {
			value, entities := renderTelegramMarkdown(test.source)
			if value != test.want {
				t.Fatalf("text = %q, want %q", value, test.want)
			}
			checkTelegramMarkdownEntities(t, value, entities)
		})
	}
}

func TestTelegramMarkdownSafeLinksAndVisibleLocalPaths(t *testing.T) {
	for _, destination := range []string{
		"/tmp/report.md", "../report.md", "file:///tmp/report.md", "javascript:alert(1)", "#section",
		"http://localhost/path", "https://example.com:99999/path", "https://example.com:0/path", "https://example.com:/path", "https://[not.an.ip]/path",
	} {
		t.Run(destination, func(t *testing.T) {
			value, entities := renderTelegramMarkdown("[label](" + destination + ")")
			if !strings.Contains(value, "label") || !strings.Contains(value, destination) {
				t.Fatalf("unsafe destination must remain visible: %q", value)
			}
			for _, entity := range entities {
				if entity.Type == "text_link" {
					t.Fatalf("invalid destination became a link: %+v", entity)
				}
			}
		})
	}
	value, entities := renderTelegramMarkdown("😀 [docs](https://example.com/a?x=1&amp;y=2)")
	if value != "😀 docs" || len(entities) != 1 || entities[0].Offset != 3 || entities[0].URL != "https://example.com/a?x=1&y=2" {
		t.Fatalf("HTTP link = %q %+v", value, entities)
	}
	checkTelegramMarkdownEntities(t, value, entities)
}

func TestTelegramMarkdownReferencesDoNotIntroduceTelegramRemovedCharacters(t *testing.T) {
	for _, reference := range []string{
		"&#13;", "&#xD;", "&#x2028;", "&#x2029;", "&#x202a;", "&#x202b;", "&#x202c;", "&#x202d;", "&#x202e;",
		"&#x0333;", "&#x033f;", "&#x030a;",
	} {
		t.Run(reference, func(t *testing.T) {
			source := "**SEC" + reference + "RET**"
			value, entities := renderTelegramMarkdown(source)
			if want := "SEC" + reference + "RET"; value != want {
				t.Fatalf("reference must stay literal: text=%q want=%q", value, want)
			}
			checkTelegramMarkdownEntities(t, value, entities)
		})
	}
	value, _ := renderTelegramMarkdown("&amp; &#65; &#x1f600;")
	if value != "& A 😀" {
		t.Fatalf("safe references should still decode: %q", value)
	}
}

func TestTelegramMarkdownTableCardsKeepValuesAndSurplusCells(t *testing.T) {
	source := "| Provider | Model | Limit | Status | Available |\n|---|---|---:|---|:---:|\n| OpenAI | `😀_model` | 128,000 | **Active** | Yes |\n| Other | model \\| pipe | 64,000 | Preview | No | SURPLUS |\n"
	value, entities := renderTelegramMarkdown(source)
	for _, want := range []string{"Provider: OpenAI", "Model: 😀_model", "Limit: 128,000", "Status: Active", "Available: Yes", "Model: model | pipe", "Additional: SURPLUS"} {
		if !strings.Contains(value, want) {
			t.Fatalf("missing %q from table: %q", want, value)
		}
	}
	if strings.Contains(value, "|---") {
		t.Fatalf("table separator remains: %q", value)
	}
	checkTelegramMarkdownEntities(t, value, entities)
}

func TestTelegramMarkdownListsAndQuotes(t *testing.T) {
	value, entities := renderTelegramMarkdown("3. **first**\n4. second\n   - nested\n\n- [x] done\n- [ ] pending\n\n> quote\n>\n> > nested quote")
	for _, want := range []string{"3. first", "4. second", "  • nested", "☑ done", "☐ pending", "> quote", "> > nested quote"} {
		if !strings.Contains(value, want) {
			t.Fatalf("missing %q in %q", want, value)
		}
	}
	checkTelegramMarkdownEntities(t, value, entities)
}

func TestTelegramFormattedSplitRebasesAndPreservesEntities(t *testing.T) {
	value := "😀 alpha\n\nbeta 🚀 gamma\n\ndelta"
	entities := []TelegramEntity{
		{Type: "bold", Offset: 3, Length: 16},
		{Type: "text_link", Offset: 10, Length: 4, URL: "https://example.com"},
	}
	chunks := splitTelegramFormatted(value, entities, 12)
	var joined strings.Builder
	for _, chunk := range chunks {
		joined.WriteString(chunk.Text)
		if !utf8.ValidString(chunk.Text) || telegramUTF16Len(chunk.Text) > 12 {
			t.Fatalf("bad chunk: %+v", chunk)
		}
		checkTelegramMarkdownEntities(t, chunk.Text, chunk.Entities)
	}
	if joined.String() != value || len(chunks) < 3 || chunks[0].Text != "😀 alpha\n\n" {
		t.Fatalf("chunks did not preserve text/prefer paragraphs: %+v", chunks)
	}
	if chunks[1].Entities[0].Offset != 0 {
		t.Fatalf("continuation entity not rebased: %+v", chunks[1])
	}
	code := strings.Repeat("😀x", 20)
	codeChunks := splitTelegramFormatted(code, []TelegramEntity{{Type: "pre", Length: telegramUTF16Len(code), Language: "go"}}, 7)
	for _, chunk := range codeChunks {
		if len(chunk.Entities) != 1 || chunk.Entities[0].Language != "go" || chunk.Entities[0].Length != telegramUTF16Len(chunk.Text) {
			t.Fatalf("code formatting not retained across chunks: %+v", chunk)
		}
		checkTelegramMarkdownEntities(t, chunk.Text, chunk.Entities)
	}
}

func TestTelegramFormattedDenseEntitiesSplitWithoutLoss(t *testing.T) {
	source := strings.Repeat("**x** y ", 250)
	value, entities := renderTelegramMarkdown(source)
	chunks := splitTelegramFormatted(value, entities, 4000)
	var joined strings.Builder
	entityCount := 0
	for _, chunk := range chunks {
		joined.WriteString(chunk.Text)
		entityCount += len(chunk.Entities)
		if len(chunk.Entities) > 99 || len(chunk.Entities) == 0 {
			t.Fatalf("entity budget exceeded: %d", len(chunk.Entities))
		}
		checkTelegramMarkdownEntities(t, chunk.Text, chunk.Entities)
	}
	if joined.String() != value || entityCount != len(entities) || len(chunks) < 3 {
		t.Fatalf("dense formatting lost: chunks=%d, entities=%d/%d", len(chunks), entityCount, len(entities))
	}
}

func TestTelegramMarkdownOversizedSourceFallsBackWithoutTruncation(t *testing.T) {
	source := strings.Repeat("**content** ", telegramMarkdownMaxSource/12+1)
	value, entities := renderTelegramMarkdown(source)
	if value != source || len(entities) != 0 {
		t.Fatal("oversized source must remain complete plain text")
	}
}
