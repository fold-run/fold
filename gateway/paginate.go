package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/fold-run/fold/auth"
)

// Composite federated pagination (design: snapshot-offset). Fold merges and
// policy-filters every upstream's full list, then serves it in pages whose
// cursors are offsets into that per-principal snapshot. A cursor carries a
// fingerprint of the snapshot it was minted against and a binding to its
// caller; any mismatch — snapshot refreshed, different principal, tampering
// — is answered with -32602 so clients restart the list rather than
// silently skipping or repeating items. Cursors never embed upstream
// identifiers or upstream cursors.

// listCursor is fold's minted cursor, base64url(JSON)-encoded on the wire.
type listCursor struct {
	Kind      string `json:"k"` // list method family, e.g. "tools"
	Offset    int    `json:"o"` // index into the filtered snapshot
	Gen       string `json:"g"` // snapshot fingerprint
	Principal string `json:"p"` // caller binding
}

var errBadCursor = &jsonrpc.Error{
	Code:    jsonrpc.CodeInvalidParams,
	Message: "invalid or expired cursor: restart the list from the beginning",
}

// principalCursorKey collapses the caller to a short non-reversible token.
// Anonymous callers share one bucket, mirroring policy and task ownership.
func principalCursorKey(p *auth.Principal) string {
	id := ""
	if p != nil {
		id = p.Issuer + "\x00" + p.Subject
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

// snapshotGen fingerprints the filtered snapshot a cursor walks.
func snapshotGen(kind string, names []string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	for _, n := range names {
		h.Write([]byte{0})
		h.Write([]byte(n))
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}

func encodeCursor(c listCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(raw string) (listCursor, bool) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return listCursor{}, false
	}
	var c listCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return listCursor{}, false
	}
	return c, true
}

// paginate slices one page out of the merged, filtered snapshot. size <= 0
// disables pagination: the whole snapshot is one page and any client-sent
// cursor is invalid (fold never minted one). name yields each item's
// identity for the snapshot fingerprint.
func paginate[T any](items []T, name func(T) string, kind, rawCursor string, size int, principal *auth.Principal) (page []T, next string, err *jsonrpc.Error) {
	if size <= 0 {
		if rawCursor != "" {
			return nil, "", errBadCursor
		}
		return items, "", nil
	}

	names := make([]string, len(items))
	for i, it := range items {
		names[i] = name(it)
	}
	gen := snapshotGen(kind, names)
	pkey := principalCursorKey(principal)

	offset := 0
	if rawCursor != "" {
		c, ok := decodeCursor(rawCursor)
		if !ok || c.Kind != kind || c.Gen != gen || c.Principal != pkey ||
			c.Offset <= 0 || c.Offset > len(items) {
			return nil, "", errBadCursor
		}
		offset = c.Offset
	}

	end := min(offset+size, len(items))
	page = items[offset:end]
	if end < len(items) {
		next = encodeCursor(listCursor{Kind: kind, Offset: end, Gen: gen, Principal: pkey})
	}
	return page, next, nil
}
