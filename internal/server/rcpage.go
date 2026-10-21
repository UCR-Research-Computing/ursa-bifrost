package server

// RC website look (UCR Research Computing, rc.css colours and type) for the few HTML pages
// people see: sign-in, the front page, not-found. Styles are inline because the pages keep
// a strict CSP (default-src 'none'). Fira Sans is used when installed, else a close system font.

import (
	"html/template"
	"net/http"
	"strings"
)

const rcCSS = `body{margin:0;font-family:'Fira Sans','Helvetica Neue',Arial,sans-serif;color:#0f1b33;background:#f2f6fc;line-height:1.55}.u{background:#001a4f;color:#ccd8ed;font-size:13px;padding:6px 0}.u a{color:#ccd8ed;text-decoration:none;margin-right:18px}.t{background:#fff;border-bottom:1px solid #d5deec}.w{max-width:40rem;margin:0 auto;padding:0 1.25rem}.br{display:block;padding:14px 0;font-family:'Fira Sans Condensed','Fira Sans',Arial,sans-serif;font-weight:700;font-size:22px;color:#00287a;border-left:4px solid #ffb81c;padding-left:12px;margin:12px 0;line-height:1.1}.br small{display:block;font-family:'Fira Sans',Arial,sans-serif;font-weight:400;font-size:12px;color:#4b5874;margin-top:3px}.c{background:#fff;border:1px solid #d5deec;border-top:4px solid #003da5;border-radius:6px;padding:1.4rem 1.5rem;margin:2rem auto}h1{font-family:'Fira Sans Condensed','Fira Sans',Arial,sans-serif;font-size:1.6rem;color:#00287a;margin:0 0 .8rem}a{color:#1565c0}code{font-family:'Fira Code',ui-monospace,monospace;background:#eef1f6;padding:0 .3rem;border-radius:3px;word-break:break-all}a.b{display:inline-block;background:#003da5;color:#fff;padding:10px 18px;border-radius:4px;text-decoration:none;font-family:'Fira Sans Condensed','Fira Sans',Arial,sans-serif;font-weight:600;text-transform:uppercase;letter-spacing:.6px;font-size:14px}.n{background:#fffaf0;border-left:4px solid #ffb81c;padding:.6rem .9rem;border-radius:0 6px 6px 0}.bad{border-top-color:#b00020}.f{color:#4b5874;font-size:13px;text-align:center;padding:0 0 2rem}.f a{color:#4b5874}`

var rcTmpl = template.Must(template.New("rc").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="theme-color" content="#00287a">
<title>{{.Title}} | {{.Brand}}</title><style>` + rcCSS + `</style></head><body>
<div class="u"><div class="w"><a href="https://www.ucr.edu/">UCR Home</a><a href="https://ucr-research-computing.github.io/">Research Computing</a></div></div>
<div class="t"><div class="w"><span class="br">{{.Brand}}<small>UCR Research Computing</small></span></div></div>
<div class="w"><div class="c{{if .Bad}} bad{{end}}"><h1>{{.Title}}</h1>{{.Body}}</div>
<p class="f"><a href="https://ucr-research-computing.github.io/help/">Help from Research Computing</a></p></div>
</body></html>`))

// rcPage writes one page in the RC look. body is trusted HTML built by the caller with
// html/template or fixed strings; never pass request text into it unescaped.
func rcPage(w http.ResponseWriter, status int, brand, title string, body template.HTML, bad bool) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	w.WriteHeader(status)
	_ = rcTmpl.Execute(w, map[string]any{"Brand": brand, "Title": title, "Body": body, "Bad": bad})
}

// wantsHTML is true for a browser; programs keep the plain-text answers.
func wantsHTML(r *http.Request) bool { return strings.Contains(r.Header.Get("Accept"), "text/html") }
