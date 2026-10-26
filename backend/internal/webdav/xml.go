package webdav

// The XML of RFC 4918 §14, on the standard encoding/xml: requests are
// decoded by namespace URI, responses written out by hand, so that every
// DAV: element carries the "D:" prefix. Windows' Mini-Redirector ignores
// elements in a default namespace (golang.org/issue/11177), which is why
// x/net/webdav carries a fork of encoding/xml — not importable from here.

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

const (
	davNS = "DAV:"
	// ocNS is where ownCloud and Nextcloud put their properties; clients
	// made for them ask for these by this namespace.
	ocNS = "http://owncloud.org/ns"
)

var (
	errInvalidPropfind  = errors.New("webdav: invalid propfind")
	errInvalidProppatch = errors.New("webdav: invalid proppatch")
	errInvalidLockInfo  = errors.New("webdav: invalid lock info")
	errUnsupportedLock  = errors.New("webdav: only exclusive write locks are supported")
)

// maxRequestXML bounds a PROPFIND, PROPPATCH or LOCK body.
const maxRequestXML = 1 << 20

// countingReader tells an empty body, which means something of its own,
// from a malformed one.
type countingReader struct {
	n int
	r io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// next returns the next token that isn't a comment, processing
// instruction or directive, which RFC 4918 says to ignore.
func next(d *xml.Decoder) (xml.Token, error) {
	for {
		t, err := d.Token()
		if err != nil {
			return t, err
		}
		switch t.(type) {
		case xml.Comment, xml.Directive, xml.ProcInst:
			continue
		}
		return t, nil
	}
}

// propNames are the names in a DAV:prop of a PROPFIND.
type propNames []xml.Name

func (pn *propNames) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		t, err := next(d)
		if err != nil {
			return err
		}
		switch t := t.(type) {
		case xml.EndElement:
			if len(*pn) == 0 {
				return fmt.Errorf("%s must not be empty", start.Name.Local)
			}
			return nil
		case xml.StartElement:
			*pn = append(*pn, t.Name)
			if err := d.Skip(); err != nil {
				return err
			}
		}
	}
}

// propfind is a PROPFIND body (§14.20).
type propfind struct {
	XMLName  xml.Name  `xml:"DAV: propfind"`
	Allprop  *struct{} `xml:"DAV: allprop"`
	Propname *struct{} `xml:"DAV: propname"`
	Prop     propNames `xml:"DAV: prop"`
	Include  propNames `xml:"DAV: include"`
}

// wantsDead reports whether answering needs the dead properties.
func (pf propfind) wantsDead() bool {
	if pf.Prop == nil {
		return true
	}
	for _, name := range pf.Prop {
		if _, live := liveProps[name]; !live {
			return true
		}
	}
	return false
}

// wants reports whether answering needs the value of a live property.
func (pf propfind) wants(name xml.Name) bool {
	if pf.Allprop != nil {
		return liveProps[name].allprop || slices.Contains(pf.Include, name)
	}
	return slices.Contains(pf.Prop, name)
}

func readPropfind(body io.Reader) (propfind, int, error) {
	c := &countingReader{r: io.LimitReader(body, maxRequestXML)}
	var pf propfind
	if err := xml.NewDecoder(c).Decode(&pf); err != nil {
		if errors.Is(err, io.EOF) && c.n == 0 {
			// An empty body asks for allprop (§9.1).
			return propfind{Allprop: &struct{}{}}, 0, nil
		}
		return propfind{}, http.StatusBadRequest, err
	}
	switch {
	case pf.Allprop == nil && pf.Include != nil,
		pf.Allprop != nil && (pf.Prop != nil || pf.Propname != nil),
		pf.Prop != nil && pf.Propname != nil,
		pf.Propname == nil && pf.Allprop == nil && pf.Prop == nil:
		return propfind{}, http.StatusBadRequest, errInvalidPropfind
	}
	return pf, 0, nil
}

// patch is one property a PROPPATCH sets (with its value as
// self-contained XML) or removes.
type patch struct {
	name   xml.Name
	value  []byte
	remove bool
}

// readProppatch reads a propertyupdate (§14.19): set and remove
// instructions, in document order.
func readProppatch(body io.Reader) ([]patch, int, error) {
	d := xml.NewDecoder(io.LimitReader(body, maxRequestXML))
	var patches []patch
	depth := 0
	var op string
	for {
		t, err := next(d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		switch t := t.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 1 && t.Name == xml.Name{Space: davNS, Local: "propertyupdate"}:
			case depth == 2 && t.Name.Space == davNS && (t.Name.Local == "set" || t.Name.Local == "remove"):
				op = t.Name.Local
			case depth == 3 && t.Name == xml.Name{Space: davNS, Local: "prop"}:
			case depth == 4:
				value, err := innerXML(d)
				if err != nil {
					return nil, http.StatusBadRequest, err
				}
				depth--
				if op == "remove" && len(bytes.TrimSpace(value)) > 0 {
					return nil, http.StatusBadRequest, errInvalidProppatch
				}
				patches = append(patches, patch{name: t.Name, value: value, remove: op == "remove"})
			default:
				return nil, http.StatusBadRequest, errInvalidProppatch
			}
		case xml.EndElement:
			depth--
		}
	}
	if len(patches) == 0 {
		return nil, http.StatusBadRequest, errInvalidProppatch
	}
	return patches, 0, nil
}

// innerXML re-encodes what is inside the element just started, up to its
// end, so that it declares every namespace it uses: the value then reads
// the same wherever it is written back. The decoder has resolved prefixes
// to namespace URIs, which the encoder declares again per element.
func innerXML(d *xml.Decoder) ([]byte, error) {
	var b bytes.Buffer
	e := xml.NewEncoder(&b)
	for depth := 0; ; {
		t, err := next(d)
		if err != nil {
			return nil, err
		}
		switch tt := t.(type) {
		case xml.StartElement:
			depth++
			attrs := tt.Attr[:0:0]
			for _, a := range tt.Attr {
				// Declarations are the encoder's to write.
				if a.Name.Space == "xmlns" || a.Name.Space == "" && a.Name.Local == "xmlns" {
					continue
				}
				attrs = append(attrs, a)
			}
			if tt.Name.Space == "" {
				// Not in the namespace of a re-declared parent.
				attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns"}})
			}
			t = xml.StartElement{Name: tt.Name, Attr: attrs}
		case xml.EndElement:
			if depth == 0 {
				if err := e.Flush(); err != nil {
					return nil, err
				}
				return b.Bytes(), nil
			}
			depth--
		}
		if err := e.EncodeToken(t); err != nil {
			return nil, err
		}
	}
}

// lockInfo is a LOCK body (§14.11).
type lockInfo struct {
	XMLName   xml.Name  `xml:"lockinfo"`
	Exclusive *struct{} `xml:"lockscope>exclusive"`
	Shared    *struct{} `xml:"lockscope>shared"`
	Write     *struct{} `xml:"locktype>write"`
	Owner     struct {
		InnerXML string `xml:",innerxml"`
	} `xml:"owner"`
}

// readLockInfo reads a LOCK body; refresh is true for an empty one, which
// refreshes a lock (§9.10.2).
func readLockInfo(body io.Reader) (li lockInfo, refresh bool, status int, err error) {
	c := &countingReader{r: io.LimitReader(body, maxRequestXML)}
	if err := xml.NewDecoder(c).Decode(&li); err != nil {
		if errors.Is(err, io.EOF) && c.n == 0 {
			return lockInfo{}, true, 0, nil
		}
		return lockInfo{}, false, http.StatusBadRequest, errInvalidLockInfo
	}
	// Exclusive write locks are what clients take in practice.
	if li.Exclusive == nil || li.Shared != nil || li.Write == nil {
		return lockInfo{}, false, http.StatusNotImplemented, errUnsupportedLock
	}
	return li, false, 0, nil
}

// property is a property in a response. value is XML ready to write
// inside its element: escaped text, or elements.
type property struct {
	name  xml.Name
	value string
}

type propstat struct {
	status int
	props  []property
}

// multistatusWriter streams a 207 Multi-Status, one response at a time.
type multistatusWriter struct {
	w   http.ResponseWriter
	buf *bufio.Writer
}

func (m *multistatusWriter) write(href string, pstats []propstat) error {
	if m.buf == nil {
		m.w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		m.w.WriteHeader(statusMulti)
		m.buf = bufio.NewWriterSize(m.w, 32<<10)
		m.buf.WriteString(xml.Header + `<D:multistatus xmlns:D="DAV:" xmlns:oc="` + ocNS + `">`)
	}
	b := m.buf
	b.WriteString("<D:response><D:href>")
	escape(b, href)
	b.WriteString("</D:href>")
	for _, ps := range pstats {
		b.WriteString("<D:propstat><D:prop>")
		for _, p := range ps.props {
			writeProp(b, p)
		}
		fmt.Fprintf(b, "</D:prop><D:status>HTTP/1.1 %d %s</D:status></D:propstat>", ps.status, statusText(ps.status))
	}
	_, err := b.WriteString("</D:response>")
	return err
}

func (m *multistatusWriter) close() error {
	if m.buf == nil {
		return nil
	}
	m.buf.WriteString("</D:multistatus>")
	return m.buf.Flush()
}

// writeProp writes one property element. Names in the namespaces the
// multistatus declares use its prefixes; any other is declared on the
// element itself, by a prefix so that its value's unprefixed elements keep
// their own namespace.
func writeProp(b *bufio.Writer, p property) {
	var open, end string
	switch p.name.Space {
	case davNS:
		open, end = "D:"+p.name.Local, "D:"+p.name.Local
	case ocNS:
		open, end = "oc:"+p.name.Local, "oc:"+p.name.Local
	case "":
		open, end = p.name.Local, p.name.Local
	default:
		var ns strings.Builder
		_ = xml.EscapeText(&ns, []byte(p.name.Space))
		open, end = `P:`+p.name.Local+` xmlns:P="`+ns.String()+`"`, "P:"+p.name.Local
	}
	if p.value == "" {
		b.WriteString("<" + open + "/>")
		return
	}
	b.WriteString("<" + open + ">" + p.value + "</" + end + ">")
}

// escape writes s as element text. Quotes stay as they are: they need no
// escaping there, and some clients compare ETags by their raw text.
func escape(w io.Writer, s string) {
	_, _ = io.WriteString(w, escapeString(s))
}

func escapeString(s string) string {
	var b strings.Builder
	// EscapeText also replaces what XML can't carry at all.
	_ = xml.EscapeText(&b, []byte(s))
	return unquote.Replace(b.String())
}

var unquote = strings.NewReplacer("&#34;", `"`, "&#39;", "'")
