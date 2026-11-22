package server

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

func artifactMarkup(source []byte) (string, error) {
	doc, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return "", err
	}
	clean := &html.Node{Type: html.DocumentNode}
	copyArtifactMarkup(clean, doc)
	var result bytes.Buffer
	if err := html.Render(&result, clean); err != nil {
		return "", err
	}
	return result.String(), nil
}

func copyArtifactMarkup(parent, input *html.Node) {
	for n := input.FirstChild; n != nil; n = n.NextSibling {
		switch n.Type {
		case html.TextNode:
			parent.AppendChild(&html.Node{Type: html.TextNode, Data: n.Data})
		case html.ElementNode:
			if n.Namespace != "" {
				continue
			}
			switch n.Data {
			case "html", "head", "body", "style", "article", "aside", "header", "footer", "main", "nav", "section", "div", "span", "p", "h1", "h2", "h3", "h4", "h5", "h6", "br", "hr", "blockquote", "strong", "b", "em", "i", "small", "pre", "code", "ul", "ol", "li", "dl", "dt", "dd", "table", "caption", "thead", "tbody", "tfoot", "tr", "th", "td", "figure", "figcaption", "img", "details", "summary", "button", "input", "label", "fieldset", "legend", "output":
			default:
				continue
			}
			child := &html.Node{Type: html.ElementNode, Data: n.Data, DataAtom: n.DataAtom}
			for _, a := range n.Attr {
				if a.Namespace == "" && artifactMarkupAttr(n.Data, a) {
					child.Attr = append(child.Attr, a)
				}
			}
			parent.AppendChild(child)
			copyArtifactMarkup(child, n)
		}
	}
}

func artifactMarkupAttr(tag string, a html.Attribute) bool {
	switch a.Key {
	case "class", "id", "style", "title", "role", "hidden", "lang", "dir", "tabindex":
		return true
	case "src":
		return tag == "img" && (strings.HasPrefix(a.Val, "data:image/png;base64,") || strings.HasPrefix(a.Val, "data:image/jpeg;base64,") || strings.HasPrefix(a.Val, "data:image/gif;base64,") || strings.HasPrefix(a.Val, "data:image/webp;base64,"))
	case "type":
		return tag == "input" && (a.Val == "checkbox" || a.Val == "radio" || a.Val == "text") || tag == "button" && a.Val == "button"
	case "name", "value", "checked", "open", "disabled", "for", "alt", "width", "height", "colspan", "rowspan", "placeholder":
		return true
	default:
		return strings.HasPrefix(a.Key, "aria-") || strings.HasPrefix(a.Key, "data-")
	}
}
