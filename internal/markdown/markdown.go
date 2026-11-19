// Package markdown renders generated documentation safely to HTML.
//
// Documentation is written by an AI from analysis of user-uploaded code, so
// the Markdown is untrusted. Rendering is configured so that it can never
// produce active content:
//
//   - raw HTML (block and inline) is dropped, not passed through;
//   - links and images with dangerous schemes (javascript:, vbscript:,
//     file:, non-image data:) get an empty URL;
//   - external links get rel="nofollow noopener noreferrer";
//   - code blocks are highlighted with CSS classes only (no inline styles,
//     so the 'self'-only Content Security Policy still applies), and every
//     token is HTML-escaped.
//
// Headings get stable, prefixed IDs and the level-2 and level-3 headings are
// returned as a table of contents.
package markdown

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"unicode"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// HeadingIDPrefix is prepended to every heading ID so document anchors
// cannot clash with the IDs of the surrounding page.
const HeadingIDPrefix = "doc-"

// Heading is one table-of-contents entry.
type Heading struct {
	Level int
	ID    string
	Text  string
}

// Result is a rendered document.
type Result struct {
	HTML template.HTML
	TOC  []Heading
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM), // tables, strikethrough, autolinks, task lists
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
		parser.WithASTTransformers(util.Prioritized(linkTransformer{}, 100)),
	),
	goldmark.WithRendererOptions(
		// html.WithUnsafe is deliberately NOT set: raw HTML is omitted and
		// dangerous URLs are blanked.
		renderer.WithNodeRenderers(util.Prioritized(codeRenderer{}, 100)),
	),
)

// Render converts Markdown to sanitized HTML and extracts its outline.
func Render(source string) (Result, error) {
	src := []byte(source)
	ctx := parser.NewContext(parser.WithIDs(newIDs()))
	doc := md.Parser().Parse(text.NewReader(src), parser.WithContext(ctx))

	var toc []Heading
	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		if h.Level >= 2 && h.Level <= 3 {
			if id, ok := h.AttributeString("id"); ok {
				if s, ok := id.([]byte); ok {
					toc = append(toc, Heading{Level: h.Level, ID: string(s), Text: plainText(h, src)})
				}
			}
		}
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		return Result{}, err
	}
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return Result{}, fmt.Errorf("render markdown: %w", err)
	}
	// The renderer escapes all text and omits raw HTML, so the output is
	// safe to embed as HTML.
	return Result{HTML: template.HTML(buf.String()), TOC: toc}, nil //nolint:gosec
}

// plainText returns the visible text of an inline container.
func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		case *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// ids generates unique, prefixed heading IDs.
type ids struct{ seen map[string]bool }

func newIDs() *ids { return &ids{seen: map[string]bool{}} }

func (s *ids) Generate(value []byte, _ ast.NodeKind) []byte {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(string(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "section"
	}
	base = HeadingIDPrefix + base
	id := base
	for i := 1; s.seen[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	s.seen[id] = true
	return []byte(id)
}

func (s *ids) Put(value []byte) { s.seen[string(value)] = true }

// linkTransformer marks external links so they don't pass referrer or
// opener information.
type linkTransformer struct{}

func (linkTransformer) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var dest []byte
		switch l := n.(type) {
		case *ast.Link:
			dest = l.Destination
		case *ast.AutoLink:
			if l.AutoLinkType != ast.AutoLinkURL {
				return ast.WalkContinue, nil
			}
			dest = []byte("https:") // autolinked URLs are always external
		default:
			return ast.WalkContinue, nil
		}
		if isExternal(string(dest)) {
			n.SetAttributeString("rel", []byte("nofollow noopener noreferrer"))
		}
		return ast.WalkContinue, nil
	})
}

func isExternal(dest string) bool {
	d := strings.ToLower(strings.TrimSpace(dest))
	return strings.HasPrefix(d, "http:") || strings.HasPrefix(d, "https:") || strings.HasPrefix(d, "//") || strings.HasPrefix(d, "mailto:")
}

// codeRenderer renders fenced and indented code blocks with chroma.
type codeRenderer struct{}

var reLanguage = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,32}$`)

var formatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.ClassPrefix("hl-"), chromahtml.PreventSurroundingPre(true))

func (codeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, renderCode)
	reg.Register(ast.KindCodeBlock, renderCode)
}

func renderCode(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var code bytes.Buffer
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		code.Write(seg.Value(src))
	}
	lang := ""
	if f, ok := n.(*ast.FencedCodeBlock); ok {
		if l := string(f.Language(src)); reLanguage.MatchString(l) {
			lang = strings.ToLower(l)
		}
	}
	_, _ = w.WriteString(`<pre class="code-block`)
	if lang != "" {
		_, _ = w.WriteString(` hl" data-lang="`)
		_, _ = w.WriteString(template.HTMLEscapeString(lang))
	}
	_, _ = w.WriteString(`"><code>`)
	if err := highlight(w, lang, code.String()); err != nil {
		_, _ = w.WriteString(template.HTMLEscapeString(code.String()))
	}
	_, _ = w.WriteString("</code></pre>\n")
	return ast.WalkSkipChildren, nil
}

// highlight writes HTML-escaped, class-annotated tokens. Unknown languages
// return an error so the caller writes plain escaped text.
func highlight(w util.BufWriter, lang, code string) error {
	if lang == "" {
		return fmt.Errorf("no language")
	}
	lexer := lexers.Get(lang)
	if lexer == nil {
		return fmt.Errorf("unknown language %q", lang)
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, styles.Fallback, it); err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}
