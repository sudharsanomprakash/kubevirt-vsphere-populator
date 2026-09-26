package vmdk

import (
	"bytes"
	"strconv"
	"strings"
)

// Extent is one line of the descriptor's extent description section.
type Extent struct {
	Access  string `json:"access"`
	Sectors uint64 `json:"sectors"`
	Type    string `json:"type"`
	File    string `json:"file"`
}

// Descriptor is the embedded text descriptor of a sparse extent.
type Descriptor struct {
	Version    string            `json:"version,omitempty"`
	CID        string            `json:"cid,omitempty"`
	ParentCID  string            `json:"parentCID,omitempty"`
	CreateType string            `json:"createType,omitempty"`
	Extents    []Extent          `json:"extents,omitempty"`
	DDB        map[string]string `json:"ddb,omitempty"`
}

// ParseDescriptor parses a descriptor. Trailing NUL padding is ignored.
func ParseDescriptor(b []byte) (Descriptor, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	d := Descriptor{DDB: map[string]string{}}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, val, ok := strings.Cut(line, "="); ok && !looksLikeExtent(line) {
			key = strings.TrimSpace(key)
			val = strings.Trim(strings.TrimSpace(val), `"`)
			switch {
			case strings.HasPrefix(key, "ddb."):
				d.DDB[key] = val
			case strings.EqualFold(key, "version"):
				d.Version = val
			case strings.EqualFold(key, "CID"):
				d.CID = val
			case strings.EqualFold(key, "parentCID"):
				d.ParentCID = val
			case strings.EqualFold(key, "createType"):
				d.CreateType = val
			}
			continue
		}
		if looksLikeExtent(line) {
			e, err := parseExtent(line)
			if err != nil {
				return d, err
			}
			d.Extents = append(d.Extents, e)
			continue
		}
		return d, formatErr("unrecognized descriptor line %q", truncate(line, 80))
	}
	return d, nil
}

func looksLikeExtent(line string) bool {
	f := strings.Fields(line)
	if len(f) < 3 {
		return false
	}
	switch f[0] {
	case "RW", "RDONLY", "NOACCESS":
		return true
	}
	return false
}

func parseExtent(line string) (Extent, error) {
	// ACCESS SIZE TYPE "FILENAME" [OFFSET]
	f := strings.Fields(line)
	sectors, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return Extent{}, formatErr("bad extent size in %q", truncate(line, 80))
	}
	e := Extent{Access: f[0], Sectors: sectors, Type: f[2]}
	if i := strings.IndexByte(line, '"'); i >= 0 {
		if j := strings.LastIndexByte(line, '"'); j > i {
			e.File = line[i+1 : j]
		}
	}
	return e, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ValidateStreamOptimized checks that the descriptor describes a single
// standalone streamOptimized sparse extent matching the header capacity.
func (d Descriptor) ValidateStreamOptimized(h Header) error {
	if !strings.EqualFold(d.CreateType, "streamOptimized") {
		return formatErr("descriptor createType is %q, want streamOptimized", d.CreateType)
	}
	if d.ParentCID != "" && !strings.EqualFold(d.ParentCID, "ffffffff") {
		return formatErr("descriptor references a parent disk (parentCID=%s); delta disks are not supported", d.ParentCID)
	}
	if len(d.Extents) != 1 {
		return formatErr("descriptor has %d extents, want exactly 1", len(d.Extents))
	}
	e := d.Extents[0]
	if !strings.EqualFold(e.Type, "SPARSE") {
		return formatErr("extent type %q, want SPARSE", e.Type)
	}
	if e.Sectors != h.Capacity {
		return formatErr("descriptor extent has %d sectors but header capacity is %d", e.Sectors, h.Capacity)
	}
	return nil
}
