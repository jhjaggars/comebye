package server

import (
	"net/http"
)

// The hub, not the helper's own permission settings, bounds what a "Chat
// about this" helper may do. An agent whose recorded role is RoleChat gets
// only the routes in chatRoutes (matched by the route's registered pattern,
// so a new route is refused to it until it is listed here); every other route
// answers 403 chat_role. Settings the user keeps for claude (allow rules, a
// hook, a mod) can widen what the helper may run in its shell, but not what
// the hub does for it.
//
// The role is looked up by the calling agent's host-qualified pane id, which
// the CLI sends from $HERDR_PANE_ID: like the rest of the agent role, it
// keeps a well-behaved helper inside its job; it is not a boundary against a
// process that withholds or forges that header (see agentauth.go).

// chatRoutes are the registered patterns a chat helper may call. The value
// says whether the route is tied to the helper's own Page (the {id} in the
// path must be that Page).
var chatRoutes = map[string]bool{
	"GET /api/whoami":                                  false,
	"GET /api/ledger":                                  false, // `comebye ledger`, `comebye read --report`
	"GET /api/agents":                                  false, // resolves a name for `comebye read`
	"GET /api/agents/{target}/transcript":              false, // `comebye read` (readGuard keeps it to this host)
	"GET /api/agents/{target}/output":                  false,
	"GET /api/hosts/{host}/agents/{target}/transcript": false,
	"GET /api/hosts/{host}/agents/{target}/output":     false,
	"GET /api/pages/{id}":                              true, // `comebye pages show`
	"POST /api/pages/{id}/note":                        true, // `comebye pages note`
}

// chatRoleOf returns the caller's chat role, if the caller is an agent whose
// recorded role is RoleChat.
func (s *Server) chatRoleOf(c caller) (Role, bool) {
	if !c.agent() || c.tagged {
		return Role{}, false
	}
	ref := c.agentRef()
	if ref == "" || s.roles == nil {
		return Role{}, false
	}
	r, ok := s.liveRoles()[ref]
	if !ok || r.Role != RoleChat {
		return Role{}, false
	}
	return r, true
}

// chatAllowed reports whether a chat helper with role may call the route.
func chatAllowed(role Role, r *http.Request) bool {
	own, ok := chatRoutes[r.Pattern]
	if !ok {
		return false
	}
	return !own || (role.Page != "" && r.PathValue("id") == role.Page)
}

func chatRoleForbidden(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "chat_role", "message": "a chat helper can only read and file a note about its Page"})
}
