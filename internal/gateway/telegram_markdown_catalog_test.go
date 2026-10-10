package gateway

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// Regression for long catalog answers: inline model identifiers and a large
// table must survive Markdown conversion and Telegram message boundaries.
func TestTelegramMarkdownLargeCatalogPreservesEveryRecord(t *testing.T) {
	var source strings.Builder
	source.WriteString("Catalog contains **80 records**. See [catalog](https://example.com/catalog).\n\n")
	source.WriteString("| Provider | Model ID | Token limit | Lifecycle | Available |\n|---|---|---:|---|:---:|\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&source, "| Provider 🧪 | `model_%03d` | 131,072 | Active | Yes |\n", i)
	}
	text, entities := renderTelegramMarkdown(source.String())
	if strings.Contains(text, "**80 records**") || strings.Contains(text, "|---") {
		t.Fatal("Markdown formatting syntax remains visible")
	}
	chunks := splitTelegramFormatted(text, entities, 3900)
	if len(chunks) < 2 {
		t.Fatalf("expected multipart catalog, got %d chunks", len(chunks))
	}
	var reconstructed strings.Builder
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk.Text) || telegramTextLength(chunk.Text) > 3900 {
			t.Fatalf("invalid Unicode or oversized chunk %d", i)
		}
		if len(chunk.Entities) > 99 {
			t.Fatalf("chunk %d leaves no room for a session header entity", i)
		}
		reconstructed.WriteString(chunk.Text)
		units := utf16.Encode([]rune(chunk.Text))
		for _, entity := range chunk.Entities {
			if entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > len(units) {
				t.Fatalf("invalid entity bounds in chunk %d: %+v", i, entity)
			}
			for _, other := range chunk.Entities {
				start, end := entity.Offset, entity.Offset+entity.Length
				otherStart, otherEnd := other.Offset, other.Offset+other.Length
				if start >= otherEnd || otherStart >= end {
					continue
				}
				if start < otherStart && otherStart < end && end < otherEnd {
					t.Fatalf("crossing entity spans in chunk %d", i)
				}
				if (entity.Type == "code" || entity.Type == "pre") && other.Type != entity.Type {
					t.Fatalf("code overlaps another entity in chunk %d: %+v %+v", i, entity, other)
				}
			}
		}
	}
	if reconstructed.String() != text {
		t.Fatal("splitting changed the rendered catalog")
	}
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("model_%03d", i)
		if strings.Count(text, id) != 1 {
			t.Fatalf("catalog record %s was lost or duplicated", id)
		}
	}
	if strings.Count(text, "131,072") != 80 || strings.Count(text, "Active") != 80 {
		t.Fatal("catalog fields were lost during table conversion")
	}
}
