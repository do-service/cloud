package webdav

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/fs"
)

// liveProp is a property computed from the node. value returns it as XML
// ready to write, ok false when the node has none (a folder's length).
type liveProp struct {
	value func(p *props, n fs.Node) (string, bool)
	// allprop: returned for allprop, as RFC 4918 §9.1 asks for its own
	// properties; the rest only when asked for by name.
	allprop bool
}

// supportedLock matches what LOCK accepts: exclusive write locks.
const supportedLock = `<D:lockentry><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockentry>`

func fileOnly(f func(n fs.Node) string) func(*props, fs.Node) (string, bool) {
	return func(_ *props, n fs.Node) (string, bool) {
		if n.Kind != fs.KindFile {
			return "", false
		}
		return f(n), true
	}
}

func always(f func(n fs.Node) string) func(*props, fs.Node) (string, bool) {
	return func(_ *props, n fs.Node) (string, bool) { return f(n), true }
}

var liveProps = map[xml.Name]liveProp{
	{Space: davNS, Local: "resourcetype"}: {allprop: true, value: always(func(n fs.Node) string {
		if n.Kind == fs.KindDir {
			return "<D:collection/>"
		}
		return ""
	})},
	{Space: davNS, Local: "displayname"}:      {allprop: true, value: always(func(n fs.Node) string { return escapeString(n.Name) })},
	{Space: davNS, Local: "getcontentlength"}: {allprop: true, value: fileOnly(func(n fs.Node) string { return strconv.FormatInt(n.Size, 10) })},
	{Space: davNS, Local: "getcontenttype"}: {allprop: true, value: fileOnly(func(n fs.Node) string {
		// From the database: the library sniffed the first 512 bytes of
		// every file, a read per child on S3.
		if n.MIME == "" {
			return "application/octet-stream"
		}
		return escapeString(n.MIME)
	})},
	{Space: davNS, Local: "getlastmodified"}: {allprop: true, value: always(func(n fs.Node) string { return n.MTime.UTC().Format(http.TimeFormat) })},
	// Folders' too: a folder's ETag changes with anything below it, which
	// is how a sync client knows to look inside.
	{Space: davNS, Local: "getetag"}:       {allprop: true, value: always(func(n fs.Node) string { return escapeString(quote(n.ETag)) })},
	{Space: davNS, Local: "supportedlock"}: {allprop: true, value: always(func(fs.Node) string { return supportedLock })},
	{Space: davNS, Local: "lockdiscovery"}: {allprop: true, value: func(p *props, n fs.Node) (string, bool) {
		var b strings.Builder
		for _, l := range p.locks[n.ID] {
			// A lock from a folder above sits that many names up.
			root := p.names[:max(len(p.names)-l.Above, 0)]
			b.WriteString(activeLock(l, p.t.href(root, l.Above > 0 || n.Kind == fs.KindDir)))
		}
		return b.String(), true
	}},
	// RFC 4331: a folder's used bytes are its size; what's available is
	// the user's quota left, absent with no quota.
	{Space: davNS, Local: "quota-used-bytes"}: {value: func(_ *props, n fs.Node) (string, bool) {
		if n.Kind != fs.KindDir {
			return "", false
		}
		return strconv.FormatInt(n.Size, 10), true
	}},
	{Space: davNS, Local: "quota-available-bytes"}: {value: func(p *props, n fs.Node) (string, bool) {
		if n.Kind != fs.KindDir {
			return "", false
		}
		left, ok := p.quotaLeft()
		return strconv.FormatInt(left, 10), ok
	}},
	// A stable id across renames and moves, which ETags and paths aren't.
	{Space: ocNS, Local: "fileid"}: {value: func(_ *props, n fs.Node) (string, bool) { return n.ID, n.ID != "" }},
	// What the user may do with it, in ownCloud's letters, by which its
	// clients mark read-only folders: S shared with them, R may reshare,
	// G read, W/CK write a file or into a folder, D N V delete, rename,
	// move.
	{Space: ocNS, Local: "permissions"}: {value: func(p *props, n fs.Node) (string, bool) {
		perms := n.Perms
		switch {
		case n.ID == "":
			return "G", true
		case n.OwnerID == p.t.user.ID:
			perms = access.All
		}
		var b strings.Builder
		if n.OwnerID != p.t.user.ID {
			b.WriteString("S")
		}
		if perms.Has(access.Reshare) {
			b.WriteString("R")
		}
		if perms.Has(access.Read) {
			b.WriteString("G")
		}
		if perms.Has(access.Write) {
			if n.Kind == fs.KindDir {
				b.WriteString("CK")
			} else {
				b.WriteString("W")
			}
			b.WriteString("DNV")
		}
		return b.String(), true
	}},
	{Space: ocNS, Local: "size"}: {value: always(func(n fs.Node) string { return strconv.FormatInt(n.Size, 10) })},
}

// props answers the properties of the nodes of one PROPFIND.
type props struct {
	files *fs.Service
	ctx   context.Context
	t     target
	// dead are the dead properties by node id, and locks the locks
	// covering each node, when the request needs them.
	dead  map[string][]fs.Prop
	locks map[string][]fs.Lock
	// names are those of the node being answered.
	names []string

	quotaOnce sync.Once
	left      int64
	limited   bool
}

func (p *props) quotaLeft() (int64, bool) {
	p.quotaOnce.Do(func() {
		used, quota, err := p.files.Quota(p.ctx, p.t.user.ID)
		if err != nil {
			slog.ErrorContext(p.ctx, "webdav: quota", "err", err)
			return
		}
		if quota != nil {
			p.left, p.limited = max(*quota-used, 0), true
		}
	})
	return p.left, p.limited
}

// find answers a PROPFIND for one node: the requested properties grouped
// into 200 and 404.
func (p *props) find(pf propfind, n fs.Node, names []string) []propstat {
	p.names = names
	found := propstat{status: http.StatusOK}
	missing := propstat{status: http.StatusNotFound}
	dead := p.dead[n.ID]
	switch {
	case pf.Propname != nil:
		for name, lp := range liveProps {
			if _, ok := lp.value(p, n); ok {
				found.props = append(found.props, property{name: name})
			}
		}
		for _, d := range dead {
			found.props = append(found.props, property{name: xml.Name{Space: d.Space, Local: d.Local}})
		}
	case pf.Allprop != nil:
		for name, lp := range liveProps {
			if !lp.allprop {
				continue
			}
			if v, ok := lp.value(p, n); ok {
				found.props = append(found.props, property{name: name, value: v})
			}
		}
		for _, d := range dead {
			found.props = append(found.props, property{name: xml.Name{Space: d.Space, Local: d.Local}, value: d.Value})
		}
		for _, name := range pf.Include {
			if lp, ok := liveProps[name]; ok && !lp.allprop {
				if v, ok := lp.value(p, n); ok {
					found.props = append(found.props, property{name: name, value: v})
				}
			}
		}
	default:
	names:
		for _, name := range pf.Prop {
			if lp, ok := liveProps[name]; ok {
				if v, ok := lp.value(p, n); ok {
					found.props = append(found.props, property{name: name, value: v})
					continue
				}
			} else {
				for _, d := range dead {
					if d.Space == name.Space && d.Local == name.Local {
						found.props = append(found.props, property{name: name, value: d.Value})
						continue names
					}
				}
			}
			missing.props = append(missing.props, property{name: name})
		}
	}
	var out []propstat
	// A response needs a propstat even when nothing was asked for.
	if len(found.props) > 0 || len(missing.props) == 0 {
		out = append(out, found)
	}
	if len(missing.props) > 0 {
		out = append(out, missing)
	}
	return out
}
