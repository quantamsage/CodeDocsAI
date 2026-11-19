package markdown

import (
	"strings"
	"testing"
)

func render(t *testing.T, src string) Result {
	t.Helper()
	r, err := Render(src)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func contains(t *testing.T, html string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(html, w) {
			t.Errorf("output missing %q\n%s", w, html)
		}
	}
}

func excludes(t *testing.T, html string, bad ...string) {
	t.Helper()
	lower := strings.ToLower(html)
	for _, b := range bad {
		if strings.Contains(lower, strings.ToLower(b)) {
			t.Errorf("output contains %q\n%s", b, html)
		}
	}
}

func TestRenderFeatures(t *testing.T) {
	src := "# Title\n\nIntro with `code` and **bold** and ~~gone~~.\n\n" +
		"## Getting started\n\n- one\n- two\n  1. nested\n\n- [x] done\n\n" +
		"| Method | Path |\n|---|---|\n| GET | `/health` |\n\n" +
		"[relative](other.md) and [site](https://example.com/x) and https://auto.example/y\n\n" +
		"### Run it\n\n```go\nfunc main() { fmt.Println(\"hi\") }\n```\n\n" +
		"```\nplain <text>\n```\n\n" +
		"> **Assumption:** inferred.\n"
	r := render(t, src)
	h := string(r.HTML)
	contains(t, h,
		`<h1 id="doc-title">Title</h1>`, `<h2 id="doc-getting-started">`, `<h3 id="doc-run-it">`,
		"<code>code</code>", "<strong>bold</strong>", "<del>gone</del>",
		"<ul>", "<ol>", `type="checkbox"`,
		"<table>", "<th>Method</th>", "<code>/health</code>",
		`<a href="other.md">relative</a>`,
		`<a href="https://example.com/x" rel="nofollow noopener noreferrer">site</a>`,
		`<a href="https://auto.example/y" rel="nofollow noopener noreferrer">`,
		`<pre class="code-block hl" data-lang="go"><code>`, `class="hl-kd"`, // keyword "func"
		`<pre class="code-block"><code>plain &lt;text&gt;`,
		"<blockquote>",
	)
	excludes(t, h, "style=")
	want := []Heading{{2, "doc-getting-started", "Getting started"}, {3, "doc-run-it", "Run it"}}
	if len(r.TOC) != len(want) {
		t.Fatalf("toc = %+v", r.TOC)
	}
	for i := range want {
		if r.TOC[i] != want[i] {
			t.Errorf("toc[%d] = %+v, want %+v", i, r.TOC[i], want[i])
		}
	}
}

func TestRenderIsSafe(t *testing.T) {
	src := "## <script>alert(1)</script> Heading\n\n" +
		"<script>alert(2)</script>\n\n<img src=x onerror=alert(3)>\n\n<iframe src=\"https://evil.example\"></iframe>\n\n" +
		"Inline <b onclick=\"alert(4)\">html</b> and <a href=\"javascript:alert(5)\">a</a>.\n\n" +
		"[js](javascript:alert(6)) [vb](VBScript:msgbox) [file](file:///etc/passwd) [data](data:text/html;base64,PHNjcmlwdD4=)\n\n" +
		"![img](javascript:alert(7))\n\n" +
		"```html\n<script>alert(8)</script>\n```\n\n" +
		"```\"><script>alert(9)</script>\nx\n```\n"
	r := render(t, src)
	h := string(r.HTML)
	excludes(t, h, "<script", "<img src=x", "<iframe", "onclick", "onerror=", `href="javascript`, `src="javascript`, `href="vbscript`, `href="file:`, `href="data:text`, "<b ")
	contains(t, h, `<span class="hl-p">&lt;</span><span class="hl-nt">script</span>`, "<!-- raw HTML omitted -->", `<a href="">js</a>`)
	for _, e := range r.TOC {
		if strings.ContainsAny(e.ID, "<>\"' ") {
			t.Errorf("unsafe heading id %q", e.ID)
		}
	}
}

func TestHeadingIDsUnique(t *testing.T) {
	r := render(t, "## Setup\n\n## Setup\n\n## !!!\n\n## Ünïcode Name\n")
	ids := []string{}
	for _, e := range r.TOC {
		ids = append(ids, e.ID)
	}
	if got := strings.Join(ids, ","); got != "doc-setup,doc-setup-1,doc-section,doc-ünïcode-name" {
		t.Errorf("ids = %s", got)
	}
}

func TestUnknownLanguageFallsBackToPlain(t *testing.T) {
	h := string(render(t, "```nosuchlang\na < b\n```\n").HTML)
	contains(t, h, `data-lang="nosuchlang"`, "a &lt; b")
}
