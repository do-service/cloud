//go:build !(darwin && cgo) && !linux

package watch

func New(string, Options) (Watcher, error) { return nil, ErrUnsupported }

func CurrentEventID() uint64 { return 0 }
