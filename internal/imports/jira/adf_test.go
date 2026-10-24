package jira

import "testing"

// doc wraps ADF block nodes (as JSON) in a document.
func doc(blocks string) string { return `{"type":"doc","version":1,"content":[` + blocks + `]}` }

func p(inlines string) string { return `{"type":"paragraph","content":[` + inlines + `]}` }

func txt(s string, marks ...string) string {
	m := ""
	for i, mk := range marks {
		if i > 0 {
			m += ","
		}
		m += mk
	}
	return `{"type":"text","text":"` + s + `","marks":[` + m + `]}`
}

const (
	strong = `{"type":"strong"}`
	em     = `{"type":"em"}`
	strike = `{"type":"strike"}`
	code   = `{"type":"code"}`
)

func li(blocks string) string { return `{"type":"listItem","content":[` + blocks + `]}` }

// TL-539: each node type Jira uses, as the Markdown TrackLin.es shows.
func TestToMarkdown(t *testing.T) {
	media := func(id, alt string) string {
		if id == "known" {
			return "https://files.example/known"
		}
		return ""
	}
	for name, c := range map[string]struct{ in, want string }{
		"empty":            {``, ``},
		"null":             {`null`, ``},
		"paragraphs":       {doc(p(txt("One")) + "," + p(txt("Two"))), "One\n\nTwo"},
		"empty paragraph":  {doc(p(txt("One")) + `,{"type":"paragraph"},` + p(txt("Two"))), "One\n\nTwo"},
		"heading":          {doc(`{"type":"heading","attrs":{"level":2},"content":[` + txt("Title") + `]}`), "## Title"},
		"heading too deep": {doc(`{"type":"heading","attrs":{"level":9},"content":[` + txt("T") + `]}`), "###### T"},
		"bold":             {doc(p(txt("a ") + "," + txt("b", strong))), "a **b**"},
		"italic":           {doc(p(txt("b", em))), "_b_"},
		"strike":           {doc(p(txt("b", strike))), "~~b~~"},
		"code":             {doc(p(txt("x := 1", code))), "`x := 1`"},
		"code with tick":   {doc(p(txt("a`b", code))), "``a`b``"},
		"code not escaped": {doc(p(txt("*p", code))), "`*p`"},
		"marks combined":   {doc(p(txt("b", strong, em, strike))), "**_~~b~~_**"},
		"spaces outside":   {doc(p(txt("a") + "," + txt(" b ", strong) + "," + txt("c"))), "a **b** c"},
		"link":             {doc(p(txt("site", `{"type":"link","attrs":{"href":"https://x.dev/a"}}`))), "[site](https://x.dev/a)"},
		"bold link":        {doc(p(txt("site", strong, `{"type":"link","attrs":{"href":"https://x.dev"}}`))), "[**site**](https://x.dev)"},
		"underline kept":   {doc(p(txt("u", `{"type":"underline"}`, `{"type":"textColor","attrs":{"color":"#f00"}}`))), "u"},
		"escaped":          {doc(p(txt(`2*3 is_six [x] \\ ` + "`" + `q`))), `2\*3 is\_six \[x\] \\ ` + "\\`q"},
		"hard break":       {doc(p(txt("a") + `,{"type":"hardBreak"},` + txt("b"))), "a\\\nb"},
		"bullets":          {doc(`{"type":"bulletList","content":[` + li(p(txt("a"))) + "," + li(p(txt("b"))) + `]}`), "- a\n- b"},
		"ordered":          {doc(`{"type":"orderedList","attrs":{"order":3},"content":[` + li(p(txt("a"))) + "," + li(p(txt("b"))) + `]}`), "3. a\n4. b"},
		"nested": {doc(`{"type":"bulletList","content":[` + li(p(txt("a"))+`,{"type":"orderedList","content":[`+li(p(txt("a1")))+","+li(p(txt("a2")))+`]}`) + "," + li(p(txt("b"))) + `]}`),
			"- a\n  1. a1\n  2. a2\n- b"},
		"tasks": {doc(`{"type":"taskList","content":[{"type":"taskItem","attrs":{"state":"DONE"},"content":[` + txt("done") + `]},{"type":"taskItem","attrs":{"state":"TODO"},"content":[` + txt("todo") + `]}]}`),
			"- [x] done\n- [ ] todo"},
		"decisions":       {doc(`{"type":"decisionList","content":[{"type":"decisionItem","content":[` + txt("Ship it") + `]}]}`), "- Ship it"},
		"code block":      {doc(`{"type":"codeBlock","attrs":{"language":"go"},"content":[` + txt(`fmt.Println(\"*\")`) + `]}`), "```go\nfmt.Println(\"*\")\n```"},
		"code block bare": {doc(`{"type":"codeBlock","content":[` + txt("a\\nb") + `]}`), "```\na\nb\n```"},
		"code fence in":   {doc(`{"type":"codeBlock","content":[` + txt("```") + `]}`), "````\n```\n````"},
		"quote":           {doc(`{"type":"blockquote","content":[` + p(txt("a")) + "," + p(txt("b")) + `]}`), "> a\n>\n> b"},
		"panel":           {doc(`{"type":"panel","attrs":{"panelType":"warning"},"content":[` + p(txt("Careful")) + `]}`), "> Careful"},
		"rule":            {doc(p(txt("a")) + `,{"type":"rule"},` + p(txt("b"))), "a\n\n---\n\nb"},
		"table": {doc(`{"type":"table","content":[
			{"type":"tableRow","content":[{"type":"tableHeader","content":[` + p(txt("Name")) + `]},{"type":"tableHeader","content":[` + p(txt("Note")) + `]}]},
			{"type":"tableRow","content":[{"type":"tableCell","content":[` + p(txt("a|b")) + `]},{"type":"tableCell","content":[` + p(txt("x", strong)) + `]}]},
			{"type":"tableRow","content":[{"type":"tableCell","content":[` + p(txt("short")) + `]}]}]}`),
			"| Name | Note |\n| --- | --- |\n| a\\|b | **x** |\n| short |  |"},
		"mention":          {doc(p(txt("hi ") + `,{"type":"mention","attrs":{"id":"5b10","text":"@Ava Smith"}}`)), "hi @Ava Smith"},
		"mention no name":  {doc(p(`{"type":"mention","attrs":{"id":"5b10"}}`)), "@someone"},
		"emoji":            {doc(p(`{"type":"emoji","attrs":{"shortName":":smile:","text":"😄"}}`)), "😄"},
		"emoji no text":    {doc(p(`{"type":"emoji","attrs":{"shortName":":party:"}}`)), ":party:"},
		"date":             {doc(p(txt("due ") + `,{"type":"date","attrs":{"timestamp":"1761264000000"}}`)), "due 2025-10-24"},
		"status":           {doc(p(`{"type":"status","attrs":{"text":"IN REVIEW","color":"blue"}}`)), "**IN REVIEW**"},
		"inline card":      {doc(p(`{"type":"inlineCard","attrs":{"url":"https://x.atlassian.net/browse/AB-1"}}`)), "<https://x.atlassian.net/browse/AB-1>"},
		"block card":       {doc(`{"type":"blockCard","attrs":{"url":"https://x.dev"}}`), "<https://x.dev>"},
		"media copied":     {doc(`{"type":"mediaSingle","content":[{"type":"media","attrs":{"id":"known","type":"file","alt":"shot.png"}}]}`), "[shot.png](https://files.example/known)"},
		"media not copied": {doc(`{"type":"mediaGroup","content":[{"type":"media","attrs":{"id":"gone","type":"file"}}]}`), "attachment"},
		"media external":   {doc(`{"type":"mediaSingle","content":[{"type":"media","attrs":{"type":"external","url":"https://x.dev/i.png"}}]}`), "[https://x.dev/i.png](https://x.dev/i.png)"},
		"media inline":     {doc(p(txt("see ") + `,{"type":"mediaInline","attrs":{"id":"known","alt":"log.txt"}}`)), "see [log.txt](https://files.example/known)"},
		"expand":           {doc(`{"type":"expand","attrs":{"title":"Details"},"content":[` + p(txt("hidden")) + `]}`), "**Details**\n\nhidden"},
		"unknown block":    {doc(`{"type":"fancyNewThing","content":[` + p(txt("kept")) + `]}`), "kept"},
		"unknown inline":   {doc(p(`{"type":"sparkle","content":[` + txt("kept too") + `]}`)), "kept too"},
		"unknown leaf":     {doc(p(`{"type":"thing","text":"leaf"}`)), "leaf"},
	} {
		got, err := ToMarkdown([]byte(c.in), media)
		if err != nil || got != c.want {
			t.Errorf("%s:\n got %q (err %v)\nwant %q", name, got, err, c.want)
		}
	}
	if _, err := ToMarkdown([]byte(`"not a doc`), nil); err == nil {
		t.Error("bad JSON should fail")
	}
}

// A description as Jira Cloud returns it (issue AB-12, trimmed): mixed blocks, marks and a mention.
func TestToMarkdownRealDocument(t *testing.T) {
	in := `{"version":1,"type":"doc","content":[
		{"type":"heading","attrs":{"level":3},"content":[{"type":"text","text":"Steps to reproduce"}]},
		{"type":"orderedList","attrs":{"order":1},"content":[
			{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"Open "},{"type":"text","text":"Settings","marks":[{"type":"strong"}]}]}]},
			{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"Click "},{"type":"text","text":"Save","marks":[{"type":"code"}]}]}]}]},
		{"type":"paragraph","content":[{"type":"text","text":"Expected: saved. Actual: "},{"type":"text","text":"500","marks":[{"type":"em"}]},{"type":"text","text":", see "},
			{"type":"text","text":"the log","marks":[{"type":"link","attrs":{"href":"https://logs.example/1"}}]},{"type":"text","text":"."}]},
		{"type":"panel","attrs":{"panelType":"info"},"content":[{"type":"paragraph","content":[{"type":"mention","attrs":{"id":"712020:abc","text":"@Sam Lee","accessLevel":""}},{"type":"text","text":" can you look? "},{"type":"emoji","attrs":{"shortName":":pray:","id":"1f64f","text":"🙏"}}]}]},
		{"type":"codeBlock","attrs":{"language":"text"},"content":[{"type":"text","text":"ERROR save failed\nat handler.go:42"}]},
		{"type":"mediaSingle","attrs":{"layout":"center"},"content":[{"type":"media","attrs":{"id":"9f1c","type":"file","collection":"","width":800,"height":600}}]}]}`
	want := "### Steps to reproduce\n\n" +
		"1. Open **Settings**\n2. Click `Save`\n\n" +
		"Expected: saved. Actual: _500_, see [the log](https://logs.example/1).\n\n" +
		"> @Sam Lee can you look? 🙏\n\n" +
		"```text\nERROR save failed\nat handler.go:42\n```\n\n" +
		"[attachment](https://files.example/9f1c)"
	got, err := ToMarkdown([]byte(in), func(id, _ string) string { return "https://files.example/" + id })
	if err != nil || got != want {
		t.Fatalf("got:\n%s\n\nwant:\n%s\n(err %v)", got, want, err)
	}
}
