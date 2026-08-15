package gateway

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP Apps is negotiated by the *client*: a server is advised to check
// `capabilities.extensions["io.modelcontextprotocol/ui"]` before registering
// UI-enabled tools, and to serve a text-only fallback to anyone who did not
// declare it. There is no reciprocal server capability, so an upstream that
// hears nothing assumes no — and fold, which declared nothing at all, made
// every upstream assume it of every caller. A host that supports apps got
// the fallback, which is the invisibility rule broken in the one direction
// nobody reports: the gateway looks like a server without apps.
//
// fold does not answer that with configuration. It carries what the client
// declared, which is what a proxy is for. The only thing that needs design is
// *where*: a named invocation rides a per-client session and can simply pass
// the declaration along, while lists ride one root session shared by every
// caller, which cannot hold two answers at once. So the root session is keyed
// by the profile below, and clients that declared different things get
// different sessions — one each in a federation where every client is alike,
// which is nearly all of them.
const (
	uiExtensionID = "io.modelcontextprotocol/ui"
	uiMimeType    = "text/html;profile=mcp-app"
)

// capProfile is the normalized shape of a client's declared extensions.
//
// It is deliberately fold's own, computed from the extension identifiers fold
// recognizes rather than from the map the client sent. A key taken straight
// from client input would let one caller declaring a thousand invented
// extension ids mint a thousand root sessions and list-cache entries per
// upstream — the failure internal/bounded exists to prevent, in a place where
// the cost is upstream connections rather than memory.
type capProfile string

const (
	// profilePlain is every client that declared nothing fold knows about,
	// and is the empty string so that a federation with no app-aware clients
	// keys exactly as it did before this existed.
	profilePlain capProfile = ""
	profileUI    capProfile = "ui"
)

// allProfiles is every profile a cache entry can exist under. It is short
// because the profile space is fold's own and holds exactly the extensions
// fold implements — one, today.
var allProfiles = []capProfile{profilePlain, profileUI}

// profileFor reads the one extension fold knows how to carry. The mime type
// is checked rather than mere presence: the extension negotiates content
// types, and a client declaring some future one is not claiming it can render
// today's.
func profileFor(caps *mcp.ClientCapabilities) capProfile {
	if caps == nil {
		return profilePlain
	}
	settings, ok := caps.Extensions[uiExtensionID].(map[string]any)
	if !ok {
		return profilePlain
	}
	mimes, ok := settings["mimeTypes"].([]any)
	if !ok {
		return profilePlain
	}
	for _, m := range mimes {
		if s, _ := m.(string); s == uiMimeType {
			return profileUI
		}
	}
	return profilePlain
}

// uiCapabilities returns the client capabilities fold declares upstream for a
// profile: the caller's own declaration, restated in fold's vocabulary rather
// than forwarded verbatim.
func (p capProfile) uiCapabilities(caps *mcp.ClientCapabilities) *mcp.ClientCapabilities {
	if p != profileUI {
		return caps
	}
	if caps == nil {
		caps = &mcp.ClientCapabilities{}
	}
	caps.AddExtension(uiExtensionID, map[string]any{"mimeTypes": []any{uiMimeType}})
	return caps
}

type capProfileKey struct{}

// withCapProfile attaches the request's capability profile to ctx, resolved
// once per request from the downstream session's initialize params.
func withCapProfile(ctx context.Context, p capProfile) context.Context {
	if p == profilePlain {
		return ctx // the zero value; nothing to carry
	}
	return context.WithValue(ctx, capProfileKey{}, p)
}

// capProfileFrom returns the request's profile, or profilePlain for any
// request that arrived without one — health probes and background loops
// included, which is the conservative answer for all of them.
func capProfileFrom(ctx context.Context) capProfile {
	p, _ := ctx.Value(capProfileKey{}).(capProfile)
	return p
}

// profileOf reads the profile a request's downstream session declared at
// initialize.
func profileOf(req mcp.Request) capProfile {
	ss, ok := req.GetSession().(*mcp.ServerSession)
	if !ok {
		return profilePlain
	}
	return profileOfSession(ss)
}

// profileOfSession is profileOf for a session held directly, as the bridge
// holds one.
func profileOfSession(ss *mcp.ServerSession) capProfile {
	if ss == nil {
		return profilePlain
	}
	init := ss.InitializeParams()
	if init == nil {
		return profilePlain
	}
	return profileFor(init.Capabilities)
}

// qualify scopes a per-upstream cache key to this profile, so two profiles
// cannot serve each other's lists — including through Redis, where the key is
// shared by a whole fleet.
func (p capProfile) qualify(key string) string {
	if p == profilePlain {
		return key
	}
	return key + "\x00" + string(p)
}
