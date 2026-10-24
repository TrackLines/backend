// Package jira reads what a Jira Cloud import needs (TL-472).
package jira

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Node is one node of Atlassian Document Format, the JSON Jira keeps descriptions and comments in.
type Node struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Marks   []Mark         `json:"marks"`
	Content []Node         `json:"content"`
}

type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

// Media turns an attachment's media id (and its alt text) into a link to the copied file; "" when
// there isn't one, and the alt text (or "attachment") is kept instead.
type Media func(id, alt string) string

// ToMarkdown converts an ADF document to the Markdown TrackLin.es uses (TL-539). Anything it doesn't
// recognise keeps its text rather than being dropped. A null or empty document is "".
func ToMarkdown(doc []byte, media Media) (string, error) {
	if len(doc) == 0 || string(doc) == "null" {
		return "", nil
	}
	var n Node
	if err := json.Unmarshal(doc, &n); err != nil {
		return "", fmt.Errorf("jira: description isn't ADF: %w", err)
	}
	c := converter{media: media}
	return strings.TrimSpace(c.block(n)), nil
}

type converter struct{ media Media }

// inline nodes sit inside a paragraph or heading; everything else is a block.
var inline = map[string]bool{"text": true, "hardBreak": true, "mention": true, "emoji": true, "date": true,
	"status": true, "inlineCard": true, "mediaInline": true, "placeholder": true, "inlineExtension": true}

func (c converter) blocks(ns []Node, sep string) string {
	var out []string
	for _, n := range ns {
		if s := c.block(n); strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, sep)
}

func (c converter) block(n Node) string {
	switch n.Type {
	case "doc":
		return c.blocks(n.Content, "\n\n")
	case "paragraph":
		return c.inlines(n.Content)
	case "heading":
		level := min(max(num(n.Attrs["level"], 1), 1), 6)
		return strings.Repeat("#", level) + " " + c.inlines(n.Content)
	case "bulletList":
		return c.list(n.Content, func(int) string { return "- " })
	case "orderedList":
		start := num(n.Attrs["order"], 1)
		return c.list(n.Content, func(i int) string { return strconv.Itoa(start+i) + ". " })
	case "taskList":
		return c.list(n.Content, func(int) string { return "- " })
	case "decisionList":
		return c.list(n.Content, func(int) string { return "- " })
	case "listItem", "decisionItem":
		return c.item(n.Content)
	case "taskItem":
		box := "[ ] "
		if str(n.Attrs["state"]) == "DONE" {
			box = "[x] "
		}
		return box + c.item(n.Content)
	case "codeBlock":
		text := ""
		for _, t := range n.Content {
			text += t.Text
		}
		fence := "```"
		for strings.Contains(text, fence) {
			fence += "`"
		}
		return fence + str(n.Attrs["language"]) + "\n" + strings.TrimRight(text, "\n") + "\n" + fence
	case "blockquote", "panel": // panels (info, warning…) have no Markdown of their own: quotes
		return quote(c.blocks(n.Content, "\n\n"))
	case "rule":
		return "---"
	case "table":
		return c.table(n)
	case "mediaSingle", "mediaGroup":
		return c.blocks(n.Content, "\n")
	case "media":
		return c.mediaLink(n)
	case "expand", "nestedExpand":
		body := c.blocks(n.Content, "\n\n")
		if title := str(n.Attrs["title"]); title != "" {
			return "**" + escape(title) + "**\n\n" + body
		}
		return body
	case "blockCard", "embedCard":
		return link(str(n.Attrs["url"]))
	}
	if inline[n.Type] {
		return c.inlines([]Node{n})
	}
	// unknown: keep its text, however deep (a new node type, an extension…)
	if len(n.Content) > 0 && inline[n.Content[0].Type] {
		return c.inlines(n.Content)
	}
	if len(n.Content) > 0 {
		return c.blocks(n.Content, "\n\n")
	}
	return escape(n.Text)
}

// list renders each item after its marker, the item's later lines indented under its first.
func (c converter) list(items []Node, marker func(i int) string) string {
	var out []string
	for i, it := range items {
		m := marker(i)
		lines := strings.Split(c.block(it), "\n")
		for j := 1; j < len(lines); j++ {
			if lines[j] != "" {
				lines[j] = strings.Repeat(" ", len(m)) + lines[j]
			}
		}
		out = append(out, m+strings.Join(lines, "\n"))
	}
	return strings.Join(out, "\n")
}

// item: a list item's blocks, a tight list (a nested list follows its text on the next line).
// ponytail: two paragraphs in one item join as one; loose lists if imports show they matter.
func (c converter) item(ns []Node) string {
	if len(ns) > 0 && inline[ns[0].Type] { // task and decision items hold text directly
		return c.inlines(ns)
	}
	return c.blocks(ns, "\n")
}

func (c converter) table(n Node) string {
	var rows [][]string
	for _, r := range n.Content {
		var cells []string
		for _, cell := range r.Content {
			s := strings.ReplaceAll(c.blocks(cell.Content, " "), "\n", " ")
			cells = append(cells, strings.ReplaceAll(s, "|", `\|`))
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		return ""
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r))
	}
	line := func(cells []string) string {
		for len(cells) < width {
			cells = append(cells, "")
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	// Markdown tables need a header: the first row is it, header cells or not
	out := []string{line(rows[0]), line(slices.Repeat([]string{"---"}, width))}
	for _, r := range rows[1:] {
		out = append(out, line(r))
	}
	return strings.Join(out, "\n")
}

func (c converter) mediaLink(n Node) string {
	alt := str(n.Attrs["alt"])
	if c.media != nil {
		if url := c.media(str(n.Attrs["id"]), alt); url != "" {
			return "[" + escape(cmp.Or(alt, "attachment")) + "](" + url + ")"
		}
	}
	if u := str(n.Attrs["url"]); u != "" { // external media
		return "[" + escape(cmp.Or(alt, u)) + "](" + u + ")"
	}
	return escape(cmp.Or(alt, "attachment"))
}

func (c converter) inlines(ns []Node) string {
	var b strings.Builder
	for _, n := range ns {
		switch n.Type {
		case "text":
			b.WriteString(marked(n.Text, n.Marks))
		case "hardBreak":
			b.WriteString("\\\n")
		case "mention": // the person's name, as Jira shows it (@Ava)
			b.WriteString(escape(cmp.Or(str(n.Attrs["text"]), "@someone")))
		case "emoji":
			b.WriteString(cmp.Or(str(n.Attrs["text"]), str(n.Attrs["shortName"])))
		case "date":
			b.WriteString(date(n.Attrs["timestamp"]))
		case "status":
			b.WriteString("**" + escape(str(n.Attrs["text"])) + "**")
		case "inlineCard":
			b.WriteString(link(str(n.Attrs["url"])))
		case "mediaInline", "media":
			b.WriteString(c.mediaLink(n))
		default:
			b.WriteString(c.block(n))
		}
	}
	return b.String()
}

// marked wraps text in its marks: code innermost, a link outermost. Spaces at either end go outside
// the markers, where Markdown needs them.
func marked(text string, marks []Mark) string {
	if text == "" {
		return ""
	}
	core := strings.TrimSpace(text)
	if core == "" {
		return text
	}
	lead := text[:strings.Index(text, core)]
	trail := text[len(lead)+len(core):]
	has := map[string]Mark{}
	for _, m := range marks {
		has[m.Type] = m
	}
	s := escape(core)
	if _, ok := has["code"]; ok {
		tick := "`"
		for strings.Contains(core, tick) {
			tick += "`"
		}
		s = tick + core + tick
		if strings.HasPrefix(core, "`") || strings.HasSuffix(core, "`") {
			s = tick + " " + core + " " + tick
		}
	}
	if _, ok := has["strike"]; ok {
		s = "~~" + s + "~~"
	}
	if _, ok := has["em"]; ok {
		s = "_" + s + "_"
	}
	if _, ok := has["strong"]; ok {
		s = "**" + s + "**"
	}
	if m, ok := has["link"]; ok { // underline, colours, sub/superscript have no Markdown: their text stays
		if href := str(m.Attrs["href"]); href != "" {
			s = "[" + s + "](" + strings.ReplaceAll(href, ")", "%29") + ")"
		}
	}
	return lead + s + trail
}

var special = regexp.MustCompile("([\\\\`*_\\[\\]])")

// escape keeps Jira's text literal where Markdown would read it as formatting.
// ponytail: inline characters only; text that starts a line with "1. " or "# " can still become a
// list or heading. Escape line starts too if imports show it.
func escape(s string) string { return special.ReplaceAllString(s, `\$1`) }

func quote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}

func link(url string) string {
	if url == "" {
		return ""
	}
	return "<" + url + ">"
}

// date: ADF dates are milliseconds since the epoch (as a string), shown as the UTC day.
func date(v any) string {
	ms, err := strconv.ParseInt(str(v), 10, 64)
	if err != nil {
		return str(v)
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func num(v any, fallback int) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return fallback
}
