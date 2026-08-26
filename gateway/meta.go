package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connection-owned `_meta` keys describe the connection a request arrived on,
// not the request. Behind a gateway there are two connections — the caller's
// and the one fold holds to the upstream — so a key that was true on the way
// in is false on the way out, and forwarding it tells the upstream something
// about a connection it is not on.
//
// The SDK makes this consequential in both directions. Its client fills these
// keys on an outgoing request only when they are *absent*
// (mcp.ClientSession.sendRequest), so a value forwarded from the caller wins
// over the one fold negotiated; and its server prefers the per-request `_meta`
// to the session's initialize params (ServerRequest.ClientCapabilities and
// friends), so the upstream believes what it is handed. The result is an
// upstream told the wrong protocol version, the wrong client identity, and
// capabilities fold has no session to bridge.
//
// fold already refuses to forward a client's raw declaration at initialize —
// capProfile in uicapability.go restates it in fold's own vocabulary,
// deliberately, so a caller cannot mint sessions by inventing extension ids.
// These keys are the same argument on the per-request path, which that guard
// does not cover.
//
// This is not the invisibility rule bending: fold is not rewriting the
// caller's request, it is declining to relay a description of a connection
// that ended at the gateway. Every other `_meta` key — progress tokens, trace
// context, task and app metadata, vendor keys — passes through untouched.
var connectionRequestMetaKeys = [...]string{
	mcp.MetaKeyProtocolVersion,
	mcp.MetaKeyClientInfo,
	mcp.MetaKeyClientCapabilities,
}

// metaKeyPrefix is the namespace every connection-owned key shares. A
// substring test over the raw params rules all of them out at once, which is
// what keeps the task path (which forwards bytes, not structs) off the
// unmarshal-remarshal road in the common case.
//
// The test alone is not sound, and the reason is worth stating: JSON lets any
// character in a key be written as an escape, so
// "\u0069o.modelcontextprotocol/protocolVersion" decodes to exactly the key
// this package removes while containing none of its bytes. A caller who wants
// the check skipped only has to escape one letter. See skipRawScan for the
// condition that closes it.
const metaKeyPrefix = "io.modelcontextprotocol/"

// sanitizeRequestMeta returns meta without the connection-owned keys.
//
// The input is returned unchanged when it carries none of them, which is every
// request from a conforming client on today's protocol era — so the proxy path
// keeps its allocation profile and only a caller that actually sent one pays
// for the copy. The input is never mutated: it belongs to the caller's request,
// which audit and the decision hook still read afterwards.
func sanitizeRequestMeta(meta mcp.Meta) mcp.Meta {
	if len(meta) == 0 {
		return meta
	}
	found := false
	for _, k := range connectionRequestMetaKeys {
		if _, ok := meta[k]; ok {
			found = true
			break
		}
	}
	if !found {
		return meta
	}
	out := make(mcp.Meta, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	for _, k := range connectionRequestMetaKeys {
		delete(out, k)
	}
	return out
}

// sanitizeRawMeta is sanitizeRequestMeta for a params blob fold forwards as
// opaque JSON — the task methods, which the SDK does not model and therefore
// does not stamp connection keys onto itself.
//
// A byte scan gates the parse: a blob with no key in the namespace is handed
// back as-is, so the ordinary task call costs one Contains and no allocation.
// A blob fold cannot parse is also handed back unchanged, because refusing a
// task call over an unreadable `_meta` would be a new failure mode on a path
// whose contract is that fold does not interpret it.
func sanitizeRawMeta(raw json.RawMessage) json.RawMessage {
	if skipRawScan(raw) {
		return raw
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return raw
	}
	rawMeta, ok := params["_meta"]
	if !ok {
		return raw
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		return raw
	}
	for _, k := range connectionRequestMetaKeys {
		delete(meta, k)
	}
	// Re-marshal unconditionally from here, even when nothing was deleted.
	//
	// Returning the original bytes on a "nothing found" result would reopen
	// the hole this function exists to close, by way of duplicate members:
	// `{"_meta":{<connection key>},"_meta":{"x":1}}` decodes — under Go's
	// last-one-wins rule — to a map with no connection key, so the check
	// passes and the original bytes go out carrying the first `_meta`
	// untouched. An upstream whose parser takes the first member instead
	// then reads exactly the key fold believed it had removed. Re-marshalling
	// collapses duplicates to the one member fold actually inspected, so what
	// the upstream parses is what fold checked, whatever its parser does with
	// duplicates.
	//
	// This costs nothing in the common case: the fast path above has already
	// returned for any blob that cannot contain one of these keys.
	if len(meta) == 0 {
		delete(params, "_meta")
	} else {
		encoded, err := json.Marshal(meta)
		if err != nil {
			return raw
		}
		params["_meta"] = encoded
	}
	out, err := json.Marshal(params)
	if err != nil {
		return raw
	}
	return out
}

// skipRawScan reports whether a params blob provably carries no
// connection-owned key, so the parse can be skipped.
//
// Two conditions, and the second is the one that makes it sound: the literal
// prefix must be absent, *and* the blob must contain no backslash. A backslash
// is the only way a JSON document can spell a character as something other
// than itself, so its absence means the bytes are the string — and a prefix
// missing from the bytes is therefore missing from every decoded key. When a
// backslash is present anywhere, escaping is possible and the blob goes down
// the parse path, where the decoded keys are compared for real.
//
// The cost of that conservatism is that a task whose params happen to contain
// an escape sequence anywhere — a Windows path in an argument, a quoted string
// — pays a parse it did not need. That is the right side to err on: the parse
// is correct and merely wasteful, while trusting the substring test is fast
// and wrong.
func skipRawScan(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Contains(raw, []byte(metaKeyPrefix)) {
		return false
	}
	return !bytes.ContainsRune(raw, '\\')
}

// stripResultMeta drops the connection-owned key an upstream stamps on its
// responses.
//
// It covers the typed result paths, which reach it through tagUpstream, plus
// completion, which does not tag and so calls it directly. The raw task path
// is deliberately excluded: its results are opaque bytes by contract, and
// parsing every one to remove a key would put a decode on a path whose whole
// promise is that fold does not interpret it. The consequence is written down
// rather than left to be discovered — an upstream's serverInfo survives on a
// task result, and because the raw result is marshalled as-is, fold's own
// identity is not stamped there either. Revisit if the task wire types stop
// being opaque. It is the reverse direction of the same rule: the upstream is
// identifying itself on its own connection, and relaying that would tell the
// caller it is talking to a server it never connected to.
func stripResultMeta(meta *mcp.Meta) {
	if *meta == nil {
		return
	}
	delete(*meta, mcp.MetaKeyServerInfo)
}
